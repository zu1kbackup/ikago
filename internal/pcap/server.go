package pcap

import (
	"errors"
	"fmt"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"ikago/internal/addr"
	"ikago/internal/crypto"
	"ikago/internal/log"
	"net"
	"sync"
	"time"
)

type clientIndicator struct {
	crypt crypto.Crypt
	seq   uint32
	ack   uint32
}

// Server describes the packet capture on the server side
type Server struct {
	Port           uint16
	ListenDevs     []*Device
	UpDev          *Device
	GatewayDev     *Device
	Crypt          crypto.Crypt
	isClosed       bool
	listenConns    []*Conn
	upConn         *Conn
	cListenPackets chan connPacket
	clientLock     sync.RWMutex
	clients        map[string]*clientIndicator
	id             uint16
	nextTCPPort    uint16
	tcpPortPool    []time.Time
	nextUDPPort    uint16
	udpPortPool    []time.Time
	nextICMPv4Id   uint16
	icmpv4IdPool   []time.Time
	valueMap       map[quintuple]uint16
	natLock        sync.RWMutex
	nat            map[natGuide]*natIndicator
}

const keepAlive float64 = 30 // seconds

// NewServer returns a new pcap server
func NewServer() *Server {
	return &Server{
		listenConns:    make([]*Conn, 0),
		cListenPackets: make(chan connPacket, 1000),
		clients:        make(map[string]*clientIndicator),
		id:             0,
		tcpPortPool:    make([]time.Time, 16384),
		udpPortPool:    make([]time.Time, 16384),
		icmpv4IdPool:   make([]time.Time, 65536),
		valueMap:       make(map[quintuple]uint16),
		nat:            make(map[natGuide]*natIndicator),
	}
}

// Open implements a method opens the pcap
func (p *Server) Open() error {
	var err error

	// Verify
	if p.Port <= 0 || p.Port > 65535 {
		return fmt.Errorf("port %d out of range", p.Port)
	}
	if len(p.ListenDevs) <= 0 {
		return errors.New("missing listen device")
	}
	if p.UpDev == nil {
		return errors.New("missing upstream device")
	}
	if p.GatewayDev == nil {
		return errors.New("missing gateway")
	}

	if len(p.ListenDevs) == 1 {
		log.Infof("Listen on %s\n", p.ListenDevs[0])
	} else {
		log.Infoln("Listen on:")
		for _, dev := range p.ListenDevs {
			log.Infof("  %s\n", dev)
		}
	}
	if !p.GatewayDev.IsLoop {
		log.Infof("Route upstream from %s to %s\n", p.UpDev, p.GatewayDev)
	} else {
		log.Infof("Route upstream in %s\n", p.UpDev)
	}

	// Handles for listening
	for _, dev := range p.ListenDevs {
		var err error
		var conn *Conn

		if dev.IsLoop {
			conn, err = Dial(dev, dev, fmt.Sprintf("tcp && dst port %d", p.Port))
		} else {
			conn, err = Dial(dev, p.GatewayDev, fmt.Sprintf("tcp && dst port %d", p.Port))
		}
		if err != nil {
			return fmt.Errorf("open listen device %s: %w", dev.Name, err)
		}

		p.listenConns = append(p.listenConns, conn)
	}

	// Handles for routing upstream
	p.upConn, err = Dial(p.UpDev, p.GatewayDev, fmt.Sprintf("((tcp || udp) && not dst port %d) || icmp", p.Port))
	if err != nil {
		return fmt.Errorf("open upstream device %s: %w", p.UpDev.Name, err)
	}

	// Start handling
	for i := 0; i < len(p.listenConns); i++ {
		conn := p.listenConns[i]

		go func() {
			for {
				packet, err := conn.ReadPacket()
				if err != nil {
					if p.isClosed {
						return
					}
					log.Errorln(fmt.Errorf("read listen in %s: %w", conn.SrcDev.Alias, err))
					continue
				}

				// Avoid conflict
				p.cListenPackets <- connPacket{packet: packet, conn: conn}
			}
		}()
	}
	go func() {
		for connPacket := range p.cListenPackets {
			err := p.handleListen(connPacket.packet, connPacket.conn)
			if err != nil {
				log.Errorln(fmt.Errorf("handle listen in %s: %w", connPacket.conn.SrcDev.Alias, err))
				log.Verboseln(connPacket.packet)
				continue
			}
		}
	}()
	for {
		packet, err := p.upConn.ReadPacket()
		if err != nil {
			if p.isClosed {
				return nil
			}
			log.Errorln(fmt.Errorf("read upstream: %w", err))
			continue
		}

		err = p.handleUpstream(packet)
		if err != nil {
			log.Errorln(fmt.Errorf("handle upstream: %w", err))
			log.Verboseln(packet)
			continue
		}
	}
}

// Close implements a method closes the pcap
func (p *Server) Close() {
	p.isClosed = true
	for _, handle := range p.listenConns {
		if handle != nil {
			handle.Close()
		}
	}
	if p.upConn != nil {
		p.upConn.Close()
	}
}

// handshake sends TCP SYN ACK to the client in handshaking
func (p *Server) handshake(indicator *packetIndicator, conn *Conn) error {
	var (
		newTransportLayer   *layers.TCP
		newNetworkLayerType gopacket.LayerType
		newNetworkLayer     gopacket.NetworkLayer
		newLinkLayerType    gopacket.LayerType
		newLinkLayer        gopacket.Layer
	)

	if indicator.transportLayerType != layers.LayerTypeTCP {
		return fmt.Errorf("transport layer type %s not support", indicator.transportLayerType)
	}

	// Initial TCP Seq
	src := indicator.src()
	client := &clientIndicator{
		crypt: p.Crypt,
		seq:   0,
		ack:   indicator.tcpLayer().Seq + 1,
	}

	// Create transport layer
	newTransportLayer = createTCPLayerSYNACK(indicator.dstPort(), indicator.srcPort(), client.seq, client.ack)

	// Decide IPv4 or IPv6
	if indicator.dstIP().To4() != nil {
		newNetworkLayerType = layers.LayerTypeIPv4
	} else {
		newNetworkLayerType = layers.LayerTypeIPv6
	}

	// Create new network layer
	var err error
	switch newNetworkLayerType {
	case layers.LayerTypeIPv4:
		newNetworkLayer, err = createNetworkLayerIPv4(indicator.dstIP(), indicator.srcIP(), p.id, 128, newTransportLayer)
	case layers.LayerTypeIPv6:
		newNetworkLayer, err = createNetworkLayerIPv6(indicator.dstIP(), indicator.srcIP(), 64, newTransportLayer)
	default:
		return fmt.Errorf("network layer type %s not support", newNetworkLayerType)

	}
	if err != nil {
		return fmt.Errorf("create network layer: %w", err)
	}

	// Decide Loopback or Ethernet
	if conn.IsLoop() {
		newLinkLayerType = layers.LayerTypeLoopback
	} else {
		newLinkLayerType = layers.LayerTypeEthernet
	}

	// Create new link layer
	switch newLinkLayerType {
	case layers.LayerTypeLoopback:
		newLinkLayer = createLinkLayerLoopback()
	case layers.LayerTypeEthernet:
		newLinkLayer, err = createLinkLayerEthernet(conn.SrcDev.HardwareAddr, conn.DstDev.HardwareAddr, newNetworkLayer)
	default:
		return fmt.Errorf("link layer type %s not support", newLinkLayerType)
	}
	if err != nil {
		return fmt.Errorf("create link layer: %w", err)
	}

	// Serialize layers
	data, err := serialize(newLinkLayer.(gopacket.SerializableLayer), newNetworkLayer.(gopacket.SerializableLayer), newTransportLayer)
	if err != nil {
		return fmt.Errorf("serialize: %w", err)
	}

	// Write packet data
	_, err = conn.Write(data)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}

	// TCP Seq
	client.seq++

	// Map client
	p.clientLock.Lock()
	p.clients[src.String()] = client
	p.clientLock.Unlock()

	// IPv4 Id
	if newNetworkLayerType == layers.LayerTypeIPv4 {
		p.id++
	}

	return nil
}

// handleListen handles TCP packets from clients
func (p *Server) handleListen(packet gopacket.Packet, conn *Conn) error {
	var (
		indicator             *packetIndicator
		embIndicator          *packetIndicator
		upValue               uint16
		newTransportLayerType gopacket.LayerType
		newTransportLayer     gopacket.Layer
		newNetworkLayerType   gopacket.LayerType
		newNetworkLayer       gopacket.NetworkLayer
		upIP                  net.IP
		newLinkLayerType      gopacket.LayerType
		newLinkLayer          gopacket.Layer
		guide                 natGuide
		ni                    *natIndicator
	)

	// Parse packet
	indicator, err := parsePacket(packet)
	if err != nil {
		return fmt.Errorf("parse packet: %w", err)
	}

	if indicator.transportLayerType != layers.LayerTypeTCP {
		return fmt.Errorf("transport layer type %s not support", indicator.transportLayerType)
	}
	src := indicator.src()

	// Handshaking with client (SYN+ACK)
	if indicator.tcpLayer().SYN {
		err := p.handshake(indicator, conn)
		if err != nil {
			return fmt.Errorf("handshake: %w", err)
		}

		log.Infof("Connect from client %s\n", src.String())

		return nil
	}

	// Empty payload (An ACK handshaking will also be recognized as empty payload)
	if len(indicator.payload()) <= 0 {
		return errors.New("empty payload")
	}

	// Client
	p.clientLock.RLock()
	client, ok := p.clients[src.String()]
	p.clientLock.RUnlock()
	if !ok {
		return fmt.Errorf("client %s unauthorized", src.String())
	}

	// Ack
	client.ack = client.ack + uint32(len(indicator.payload()))

	// Decrypt
	contents, err := client.crypt.Decrypt(indicator.payload())
	if err != nil {
		return fmt.Errorf("decrypt: %w", err)
	}

	// Parse embedded packet
	embIndicator, err = parseEmbPacket(contents)
	if err != nil {
		return fmt.Errorf("parse embedded packet: %w", err)
	}

	// Distribute port/Id by source and client address and protocol
	q := quintuple{
		src:   embIndicator.natSrc().String(),
		dst:   indicator.natSrc().String(),
		proto: embIndicator.natProto(),
	}
	upValue, ok = p.valueMap[q]
	if !ok {
		// if ICMPv4 error is not in NAT, drop it
		if embIndicator.transportLayerType == layers.LayerTypeICMPv4 && !embIndicator.icmpv4Indicator.isQuery() {
			return errors.New("missing nat")
		}
		upValue, err = p.dist(embIndicator.transportLayerType)
		if err != nil {
			return fmt.Errorf("distribute: %w", err)
		}
		p.valueMap[q] = upValue
	}

	// Create new transport layer
	newTransportLayerType = embIndicator.transportLayerType
	switch newTransportLayerType {
	case layers.LayerTypeTCP:
		tcpLayer := embIndicator.tcpLayer()
		temp := *tcpLayer
		newTransportLayer = &temp

		newTCPLayer := newTransportLayer.(*layers.TCP)

		newTCPLayer.SrcPort = layers.TCPPort(upValue)
	case layers.LayerTypeUDP:
		udpLayer := embIndicator.udpLayer()
		temp := *udpLayer
		newTransportLayer = &temp

		newUDPLayer := newTransportLayer.(*layers.UDP)

		newUDPLayer.SrcPort = layers.UDPPort(upValue)
	case layers.LayerTypeICMPv4:
		if embIndicator.icmpv4Indicator.isQuery() {
			temp := *embIndicator.icmpv4Indicator.layer
			newTransportLayer = &temp

			newICMPv4Layer := newTransportLayer.(*layers.ICMPv4)

			newICMPv4Layer.Id = upValue
		} else {
			newTransportLayer = embIndicator.icmpv4Indicator.newPureICMPv4Layer()

			newICMPv4Layer := newTransportLayer.(*layers.ICMPv4)

			temp := *embIndicator.icmpv4Indicator.embIPv4Layer
			newEmbIPv4Layer := &temp

			newEmbIPv4Layer.DstIP = conn.LocalAddr().(*addr.MultiIPAddr).IPv4()

			var err error
			var newEmbTransportLayer gopacket.Layer
			switch embIndicator.icmpv4Indicator.embTransportLayerType {
			case layers.LayerTypeTCP:
				temp := *embIndicator.icmpv4Indicator.embTransportLayer.(*layers.TCP)
				newEmbTransportLayer = &temp

				newEmbTCPLayer := newEmbTransportLayer.(*layers.TCP)

				newEmbTCPLayer.DstPort = layers.TCPPort(upValue)

				err = newEmbTCPLayer.SetNetworkLayerForChecksum(newEmbIPv4Layer)
			case layers.LayerTypeUDP:
				temp := *embIndicator.icmpv4Indicator.embTransportLayer.(*layers.UDP)
				newEmbTransportLayer = &temp

				newEmbUDPLayer := newEmbTransportLayer.(*layers.UDP)

				newEmbUDPLayer.DstPort = layers.UDPPort(upValue)

				err = newEmbUDPLayer.SetNetworkLayerForChecksum(newEmbIPv4Layer)
			case layers.LayerTypeICMPv4:
				temp := *embIndicator.icmpv4Indicator.embTransportLayer.(*layers.ICMPv4)
				newEmbTransportLayer = &temp

				if embIndicator.icmpv4Indicator.isEmbQuery() {
					newEmbICMPv4Layer := newEmbTransportLayer.(*layers.ICMPv4)

					newEmbICMPv4Layer.Id = upValue
				}
			default:
				return fmt.Errorf("create transport layer: %w", fmt.Errorf("transport layer type %s not support", embIndicator.icmpv4Indicator.embTransportLayerType))
			}
			if err != nil {
				return fmt.Errorf("create transport layer: %w", fmt.Errorf("set network layer for checksum: %w", err))
			}

			payload, err := serialize(newEmbIPv4Layer, newEmbTransportLayer.(gopacket.SerializableLayer))
			if err != nil {
				return fmt.Errorf("create transport layer: %w", fmt.Errorf("serialize: %w", err))
			}

			newICMPv4Layer.Payload = payload
		}
	default:
		return fmt.Errorf("transport layer type %s not support", newTransportLayerType)
	}

	// Create new network layer
	newNetworkLayerType = embIndicator.networkLayerType
	switch newNetworkLayerType {
	case layers.LayerTypeIPv4:
		ipv4Layer := embIndicator.networkLayer.(*layers.IPv4)
		temp := *ipv4Layer
		newNetworkLayer = &temp

		newIPv4Layer := newNetworkLayer.(*layers.IPv4)

		newIPv4Layer.SrcIP = conn.LocalAddr().(*addr.MultiIPAddr).IPv4()
		upIP = newIPv4Layer.SrcIP
	case layers.LayerTypeIPv6:
		ipv6Layer := embIndicator.networkLayer.(*layers.IPv6)
		temp := *ipv6Layer
		newNetworkLayer = &temp

		newIPv6Layer := newNetworkLayer.(*layers.IPv6)

		newIPv6Layer.SrcIP = conn.LocalAddr().(*addr.MultiIPAddr).IPv6()
		upIP = newIPv6Layer.SrcIP
	default:
		return fmt.Errorf("network layer type %s not support", newNetworkLayerType)
	}

	// Set network layer for transport layer
	switch newTransportLayerType {
	case layers.LayerTypeTCP:
		tcpLayer := newTransportLayer.(*layers.TCP)

		err = tcpLayer.SetNetworkLayerForChecksum(newNetworkLayer)
	case layers.LayerTypeUDP:
		udpLayer := newTransportLayer.(*layers.UDP)

		err = udpLayer.SetNetworkLayerForChecksum(newNetworkLayer)
	case layers.LayerTypeICMPv4:
		break
	default:
		return fmt.Errorf("transport layer type %s not support", newTransportLayerType)
	}
	if err != nil {
		return fmt.Errorf("set network layer for checksum: %w", err)
	}

	// Decide Loopback or Ethernet
	if conn.IsLoop() {
		newLinkLayerType = layers.LayerTypeLoopback
	} else {
		newLinkLayerType = layers.LayerTypeEthernet
	}

	// Create new link layer
	switch newLinkLayerType {
	case layers.LayerTypeLoopback:
		newLinkLayer = createLinkLayerLoopback()
	case layers.LayerTypeEthernet:
		newLinkLayer, err = createLinkLayerEthernet(conn.SrcDev.HardwareAddr, conn.DstDev.HardwareAddr, newNetworkLayer)
	default:
		return fmt.Errorf("link layer type %s not support", newLinkLayerType)
	}
	if err != nil {
		return fmt.Errorf("create link layer: %w", err)
	}

	// Serialize layers
	data, err := serialize(newLinkLayer.(gopacket.SerializableLayer),
		newNetworkLayer.(gopacket.SerializableLayer),
		newTransportLayer.(gopacket.SerializableLayer),
		gopacket.Payload(embIndicator.payload()))
	if err != nil {
		return fmt.Errorf("serialize: %w", err)
	}

	// Write packet data
	n, err := conn.Write(data)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}

	// Record the source and the source device of the packet
	var addNAT bool
	switch newTransportLayerType {
	case layers.LayerTypeTCP:
		a := net.TCPAddr{
			IP:   upIP,
			Port: int(upValue),
		}
		guide = natGuide{
			src:   a.String(),
			proto: newTransportLayerType,
		}
		addNAT = true
	case layers.LayerTypeUDP:
		a := net.UDPAddr{
			IP:   upIP,
			Port: int(upValue),
		}
		guide = natGuide{
			src:   a.String(),
			proto: newTransportLayerType,
		}
		addNAT = true
	case layers.LayerTypeICMPv4:
		if embIndicator.icmpv4Indicator.isQuery() {
			guide = natGuide{
				src: addr.ICMPQueryAddr{
					IP: upIP,
					Id: upValue,
				}.String(),
				proto: newTransportLayerType,
			}
			addNAT = true
		}
	default:
		return fmt.Errorf("transport layer type %s not support", newTransportLayerType)
	}
	if addNAT {
		ni = &natIndicator{
			src:    src,
			dst:    indicator.dst(),
			embSrc: embIndicator.natSrc(),
			conn:   conn,
		}
		p.natLock.Lock()
		p.nat[guide] = ni
		p.natLock.Unlock()
	}

	// Keep alive
	proto := embIndicator.natProto()
	switch proto {
	case layers.LayerTypeTCP:
		p.tcpPortPool[convertFromPort(upValue)] = time.Now()
	case layers.LayerTypeUDP:
		p.udpPortPool[convertFromPort(upValue)] = time.Now()
	case layers.LayerTypeICMPv4:
		p.icmpv4IdPool[upValue] = time.Now()
	default:
		return fmt.Errorf("protocol type %s not support", proto)
	}

	log.Verbosef("Redirect an inbound %s packet: %s -> %s (%d Bytes)\n",
		embIndicator.transportLayerType, embIndicator.src(), embIndicator.dst(), n)

	return nil
}

// handleUpstream handles TCP and UDP packets from destinations
func (p *Server) handleUpstream(packet gopacket.Packet) error {
	var (
		indicator             *packetIndicator
		embTransportLayerType gopacket.LayerType
		embTransportLayer     gopacket.Layer
		embNetworkLayerType   gopacket.LayerType
		embNetworkLayer       gopacket.NetworkLayer
		newTransportLayer     gopacket.SerializableLayer
		newNetworkLayer       gopacket.SerializableLayer
		newLinkLayer          gopacket.SerializableLayer
	)

	// Parse packet
	indicator, err := parsePacket(packet)
	if err != nil {
		return fmt.Errorf("parse packet: %w", err)
	}

	// NAT
	guide := natGuide{
		src:   indicator.natDst().String(),
		proto: indicator.transportLayerType,
	}
	p.natLock.RLock()
	ni, ok := p.nat[guide]
	p.natLock.RUnlock()
	if !ok {
		return nil
	}

	// Client
	src := ni.src
	p.clientLock.RLock()
	client, ok := p.clients[src.String()]
	p.clientLock.RUnlock()

	// Keep alive
	proto := indicator.natProto()
	switch proto {
	case layers.LayerTypeTCP:
		p.tcpPortPool[convertFromPort(indicator.dstPort())] = time.Now()
	case layers.LayerTypeUDP:
		p.udpPortPool[convertFromPort(indicator.dstPort())] = time.Now()
	case layers.LayerTypeICMPv4:
		p.icmpv4IdPool[indicator.icmpv4Indicator.id()] = time.Now()
	default:
		return fmt.Errorf("protocol type %s not support", proto)
	}

	// Create embedded transport layer
	embTransportLayerType = indicator.transportLayerType
	switch embTransportLayerType {
	case layers.LayerTypeTCP:
		embTCPLayer := indicator.transportLayer.(*layers.TCP)
		temp := *embTCPLayer
		embTransportLayer = &temp

		newEmbTCPLayer := embTransportLayer.(*layers.TCP)

		newEmbTCPLayer.DstPort = layers.TCPPort(ni.embSrc.(*net.TCPAddr).Port)
	case layers.LayerTypeUDP:
		embUDPLayer := indicator.transportLayer.(*layers.UDP)
		temp := *embUDPLayer
		embTransportLayer = &temp

		newEmbUDPLayer := embTransportLayer.(*layers.UDP)

		newEmbUDPLayer.DstPort = layers.UDPPort(ni.embSrc.(*net.UDPAddr).Port)
	case layers.LayerTypeICMPv4:
		if indicator.icmpv4Indicator.isQuery() {
			embICMPv4Layer := indicator.icmpv4Indicator.layer
			temp := *embICMPv4Layer
			embTransportLayer = &temp

			newEmbICMPv4Layer := embTransportLayer.(*layers.ICMPv4)

			newEmbICMPv4Layer.Id = ni.embSrc.(*addr.ICMPQueryAddr).Id
		} else {
			embTransportLayer = indicator.icmpv4Indicator.newPureICMPv4Layer()

			newEmbICMPv4Layer := embTransportLayer.(*layers.ICMPv4)

			temp := *indicator.icmpv4Indicator.embIPv4Layer
			newEmbEmbIPv4Layer := &temp

			newEmbEmbIPv4Layer.SrcIP = ni.EmbSrcIP()

			var err error
			var newEmbEmbTransportLayer gopacket.Layer
			switch indicator.icmpv4Indicator.embTransportLayerType {
			case layers.LayerTypeTCP:
				temp := *indicator.icmpv4Indicator.embTransportLayer.(*layers.TCP)
				newEmbEmbTransportLayer = &temp

				newEmbEmbTCPLayer := newEmbEmbTransportLayer.(*layers.TCP)

				newEmbEmbTCPLayer.SrcPort = layers.TCPPort(ni.embSrc.(*net.TCPAddr).Port)

				err = newEmbEmbTCPLayer.SetNetworkLayerForChecksum(newEmbEmbIPv4Layer)
			case layers.LayerTypeUDP:
				temp := *indicator.icmpv4Indicator.embTransportLayer.(*layers.UDP)
				newEmbEmbTransportLayer = &temp

				newEmbEmbUDPLayer := newEmbEmbTransportLayer.(*layers.UDP)

				newEmbEmbUDPLayer.SrcPort = layers.UDPPort(ni.embSrc.(*net.UDPAddr).Port)

				err = newEmbEmbUDPLayer.SetNetworkLayerForChecksum(newEmbEmbIPv4Layer)
			case layers.LayerTypeICMPv4:
				temp := *indicator.icmpv4Indicator.embTransportLayer.(*layers.ICMPv4)
				newEmbEmbTransportLayer = &temp

				if indicator.icmpv4Indicator.isEmbQuery() {
					newEmbEmbICMPv4Layer := newEmbEmbTransportLayer.(*layers.ICMPv4)

					newEmbEmbICMPv4Layer.Id = ni.embSrc.(*addr.ICMPQueryAddr).Id
				}
			default:
				return fmt.Errorf("create embedded transport layer: %w", fmt.Errorf("transport layer type %s not support", indicator.icmpv4Indicator.embTransportLayerType))
			}
			if err != nil {
				return fmt.Errorf("create embedded transport layer: %w", fmt.Errorf("set network layer for checksum: %w", err))
			}

			payload, err := serialize(newEmbEmbIPv4Layer, newEmbEmbTransportLayer.(gopacket.SerializableLayer))
			if err != nil {
				return fmt.Errorf("create embedded transport layer: %w", fmt.Errorf("serialize: %w", err))
			}

			newEmbICMPv4Layer.Payload = payload
		}
	default:
		return fmt.Errorf("embedded transport layer type %s not support", embTransportLayerType)
	}

	// Create embedded network layer
	embNetworkLayerType = indicator.networkLayerType
	switch embNetworkLayerType {
	case layers.LayerTypeIPv4:
		embIPv4Layer := indicator.networkLayer.(*layers.IPv4)
		temp := *embIPv4Layer
		embNetworkLayer = &temp

		newEmbIPv4Layer := embNetworkLayer.(*layers.IPv4)

		newEmbIPv4Layer.DstIP = ni.EmbSrcIP()
	case layers.LayerTypeIPv6:
		embIPv6Layer := indicator.networkLayer.(*layers.IPv6)
		temp := *embIPv6Layer
		embNetworkLayer = &temp

		newEmbIPv6Layer := embNetworkLayer.(*layers.IPv6)

		newEmbIPv6Layer.DstIP = ni.EmbSrcIP()
	default:
		return fmt.Errorf("embedded network layer type %s not support", embNetworkLayerType)
	}

	// Set network layer for transport layer
	switch embTransportLayerType {
	case layers.LayerTypeTCP:
		embTCPLayer := embTransportLayer.(*layers.TCP)

		err = embTCPLayer.SetNetworkLayerForChecksum(embNetworkLayer)
	case layers.LayerTypeUDP:
		embUDPLayer := embTransportLayer.(*layers.UDP)

		err = embUDPLayer.SetNetworkLayerForChecksum(embNetworkLayer)
	case layers.LayerTypeICMPv4:
		break
	default:
		return fmt.Errorf("embedded transport layer type %s not support", embTransportLayerType)
	}
	if err != nil {
		return fmt.Errorf("set embedded network layer for checksum: %w", err)
	}

	// Construct contents of new application layer
	contents, err := serialize(embNetworkLayer.(gopacket.SerializableLayer),
		embTransportLayer.(gopacket.SerializableLayer),
		gopacket.Payload(indicator.payload()))
	if err != nil {
		return fmt.Errorf("serialize embedded: %w", err)
	}

	// Wrap
	newTransportLayer, newNetworkLayer, newLinkLayer, err = wrap(uint16(ni.dst.(*net.TCPAddr).Port), uint16(src.(*net.TCPAddr).Port), client.seq, client.ack, ni.conn, src.(*net.TCPAddr).IP, p.id, indicator.ttl()-1)
	if err != nil {
		return fmt.Errorf("wrap: %w", err)
	}

	// Encrypt
	contents, err = client.crypt.Encrypt(contents)
	if err != nil {
		return fmt.Errorf("encrypt: %w", err)
	}

	// Serialize layers
	data, err := serialize(newLinkLayer.(gopacket.SerializableLayer),
		newNetworkLayer.(gopacket.SerializableLayer),
		newTransportLayer,
		gopacket.Payload(contents))
	if err != nil {
		return fmt.Errorf("serialize: %w", err)
	}

	// Write packet data
	n, err := ni.conn.Write(data)
	if err != nil {
		return fmt.Errorf("write: %w", err)
	}

	// TCP Seq
	client.seq = client.seq + uint32(len(contents))

	// IPv4 Id
	if newNetworkLayer.LayerType() == layers.LayerTypeIPv4 {
		p.id++
	}

	log.Verbosef("Redirect an outbound %s packet: %s <- %s (%d Bytes)\n",
		indicator.transportLayerType, ni.embSrc.String(), indicator.src(), n)

	return nil
}

func (p *Server) dist(t gopacket.LayerType) (uint16, error) {
	now := time.Now()

	switch t {
	case layers.LayerTypeTCP:
		for i := 0; i < 16384; i++ {
			s := p.nextTCPPort % 16384

			// Point to next port
			p.nextTCPPort++

			// Check if the port is alive
			last := p.tcpPortPool[s]
			if now.Sub(last).Seconds() > keepAlive {
				return 49152 + s, nil
			}
		}
	case layers.LayerTypeUDP:
		for i := 0; i < 16384; i++ {
			s := p.nextUDPPort % 16384

			// Point to next port
			p.nextUDPPort++

			// Check if the port is alive
			last := p.udpPortPool[s]
			if now.Sub(last).Seconds() > keepAlive {
				return 49152 + s, nil
			}
		}
	case layers.LayerTypeICMPv4:
		for i := 0; i < 65536; i++ {
			s := p.nextICMPv4Id

			// Point to next Id
			p.nextICMPv4Id++

			// Check if the Id is alive
			last := p.icmpv4IdPool[s]
			if now.Sub(last).Seconds() > keepAlive {
				return s, nil
			}
		}
	default:
		return 0, fmt.Errorf("transport layer type %s not support", t)
	}
	return 0, fmt.Errorf("%s pool empty", t)
}

func convertFromPort(port uint16) uint16 {
	return port - 49152
}
