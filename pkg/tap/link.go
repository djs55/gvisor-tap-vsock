package tap

import (
	"encoding/binary"
	"fmt"
	"io"
	"net"
	"sync"
	"sync/atomic"

	log "github.com/golang/glog"
	"github.com/google/gopacket"
	"github.com/google/gopacket/layers"
	"github.com/pkg/errors"
	"gvisor.dev/gvisor/pkg/tcpip"
	"gvisor.dev/gvisor/pkg/tcpip/buffer"
	"gvisor.dev/gvisor/pkg/tcpip/header"
	"gvisor.dev/gvisor/pkg/tcpip/stack"
)

type LinkEndpoint struct {
	Sent     uint64
	Received uint64

	Listener            net.Listener
	Debug               bool
	Mac                 tcpip.LinkAddress
	MaxTransmissionUnit int

	conn     net.Conn
	connLock sync.Mutex

	dispatcher stack.NetworkDispatcher

	writeLock sync.Mutex
}

func (e *LinkEndpoint) AddHeader(local, remote tcpip.LinkAddress, protocol tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) {
}

func (e *LinkEndpoint) ARPHardwareType() header.ARPHardwareType {
	return header.ARPHardwareEther
}

func (e *LinkEndpoint) Attach(dispatcher stack.NetworkDispatcher) {
	e.dispatcher = dispatcher
}

func (e *LinkEndpoint) Capabilities() stack.LinkEndpointCapabilities {
	return stack.CapabilityResolutionRequired | stack.CapabilityRXChecksumOffload
}

func (e *LinkEndpoint) IsAttached() bool {
	return e.dispatcher != nil
}

func (e *LinkEndpoint) LinkAddress() tcpip.LinkAddress {
	return e.Mac
}

func (e *LinkEndpoint) MaxHeaderLength() uint16 {
	return uint16(header.EthernetMinimumSize)
}

func (e *LinkEndpoint) MTU() uint32 {
	return uint32(e.MaxTransmissionUnit)
}

func (e *LinkEndpoint) Wait() {
}

func (e *LinkEndpoint) WritePackets(r *stack.Route, gso *stack.GSO, pkts stack.PacketBufferList, protocol tcpip.NetworkProtocolNumber) (int, *tcpip.Error) {
	return 1, tcpip.ErrNoRoute
}

func (e *LinkEndpoint) WritePacket(r *stack.Route, gso *stack.GSO, protocol tcpip.NetworkProtocolNumber, pkt *stack.PacketBuffer) *tcpip.Error {
	hdr := pkt.Header
	payload := pkt.Data
	eth := header.Ethernet(hdr.Prepend(header.EthernetMinimumSize))
	ethHdr := &header.EthernetFields{
		DstAddr: r.RemoteLinkAddress,
		Type:    protocol,
	}

	// Preserve the src address if it's set in the route.
	if r.LocalLinkAddress != "" {
		ethHdr.SrcAddr = r.LocalLinkAddress
	} else {
		ethHdr.SrcAddr = e.Mac
	}
	eth.Encode(ethHdr)

	if e.Debug {
		packet := gopacket.NewPacket(append(hdr.View(), payload.ToView()...), layers.LayerTypeEthernet, gopacket.Default)
		log.Info(packet.String())
	}

	if err := e.writeSockets(hdr, payload); err != nil {
		log.Error(errors.Wrap(err, "cannot send packets"))
		return tcpip.ErrAborted
	}
	return nil
}

func (e *LinkEndpoint) writeSockets(hdr buffer.Prependable, payload buffer.VectorisedView) error {
	size := make([]byte, 2)
	binary.LittleEndian.PutUint16(size, uint16(hdr.UsedLength()+payload.Size()))

	e.writeLock.Lock()
	defer e.writeLock.Unlock()

	e.connLock.Lock()
	defer e.connLock.Unlock()

	if e.conn == nil {
		return nil
	}

	if _, err := e.conn.Write(size); err != nil {
		e.conn.Close()
		e.conn = nil
		return err
	}
	if _, err := e.conn.Write(hdr.View()); err != nil {
		e.conn.Close()
		e.conn = nil
		return err
	}
	if _, err := e.conn.Write(payload.ToView()); err != nil {
		e.conn.Close()
		e.conn = nil
		return err
	}

	atomic.AddUint64(&e.Sent, uint64(hdr.UsedLength()+payload.Size()))
	return nil
}

func (e *LinkEndpoint) WriteRawPacket(vv buffer.VectorisedView) *tcpip.Error {
	return tcpip.ErrNoRoute
}

func (e *LinkEndpoint) AcceptOne() error {
	log.Info("waiting for packets...")
	for {
		conn, err := e.Listener.Accept()
		if err != nil {
			return errors.Wrap(err, "cannot accept new client")
		}
		e.connLock.Lock()
		e.conn = conn
		e.connLock.Unlock()
		go func() {
			defer func() {
				e.connLock.Lock()
				e.conn = nil
				e.connLock.Unlock()
				conn.Close()
			}()
			if err := rx(conn, e); err != nil {
				log.Error(errors.Wrap(err, "cannot receive packets"))
				return
			}
		}()
	}
}

func rx(conn net.Conn, e *LinkEndpoint) error {
	sizeBuf := make([]byte, 2)

	for {
		n, err := io.ReadFull(conn, sizeBuf)
		if err != nil {
			return errors.Wrap(err, "cannot read size from socket")
		}
		if n != 2 {
			return fmt.Errorf("unexpected size %d", n)
		}
		size := int(binary.LittleEndian.Uint16(sizeBuf[0:2]))

		buf := make([]byte, e.MaxTransmissionUnit+header.EthernetMinimumSize)
		n, err = io.ReadFull(conn, buf[:size])
		if err != nil {
			return errors.Wrap(err, "cannot read packet from socket")
		}
		if n == 0 || n != size {
			return fmt.Errorf("unexpected size %d != %d", n, size)
		}

		if e.Debug {
			packet := gopacket.NewPacket(buf[:size], layers.LayerTypeEthernet, gopacket.Default)
			log.Info(packet.String())
		}

		view := buffer.View(buf[:size])
		eth := header.Ethernet(view)
		vv := buffer.NewVectorisedView(len(view), []buffer.View{view})
		vv.TrimFront(header.EthernetMinimumSize)

		if e.dispatcher == nil {
			continue
		}
		atomic.AddUint64(&e.Received, uint64(size))
		e.dispatcher.DeliverNetworkPacket(
			eth.SourceAddress(),
			eth.DestinationAddress(),
			eth.Type(),
			&stack.PacketBuffer{
				Data: vv,
			},
		)
	}
}
