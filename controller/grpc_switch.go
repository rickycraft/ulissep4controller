package main

import (
	"context"
	"controller/pkg/client"
	"encoding/binary"
	"fmt"
	"time"

	p4_v1 "github.com/p4lang/p4runtime/go/p4/v1"
	log "github.com/sirupsen/logrus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

type GrpcSwitch struct {
	id          uint64
	binBytes    []byte
	p4infoBytes []byte
	ports       int
	addr        string
	restarts    int
	log         *log.Entry
	errCh       chan error
	ctx         context.Context
	p4RtC       *client.Client
	messageCh   chan *p4_v1.StreamMessageResponse
}

func createSwitch(ctx context.Context, deviceID uint64, binBytes []byte, p4infoBytes []byte, ports int) *GrpcSwitch {
	return &GrpcSwitch{
		id:          deviceID,
		binBytes:    binBytes,
		p4infoBytes: p4infoBytes,
		ports:       ports,
		addr:        fmt.Sprintf("%s:%d", defaultAddr, defaultPort+deviceID),
		log:         log.WithField("ID", deviceID),
		ctx:         ctx,
	}
}

func (sw *GrpcSwitch) runSwitch() error {
	sw.log.Infof("Connecting to server at %s", sw.addr)
	creds, err := credentials.NewClientTLSFromFile("/tmp/cert.pem", "")
	if err != nil {
		return err
	}
	conn, err := grpc.Dial(sw.addr, grpc.WithTransportCredentials(creds))
	if err != nil {
		return err
	}
	// checking runtime
	c := p4_v1.NewP4RuntimeClient(conn)
	resp, err := c.Capabilities(sw.ctx, &p4_v1.CapabilitiesRequest{})
	if err != nil {
		return err
	}
	sw.log.Infof("Connected, runtime version: %s", resp.P4RuntimeApiVersion)
	// create runtime client
	electionID := p4_v1.Uint128{High: 0, Low: 1}
	sw.messageCh = make(chan *p4_v1.StreamMessageResponse, 1000)
	arbitrationCh := make(chan bool)
	sw.p4RtC = client.NewClient(c, sw.id, electionID)
	go sw.p4RtC.Run(sw.ctx, arbitrationCh, sw.messageCh)
	// check primary
	for isPrimary := range arbitrationCh {
		if isPrimary {
			log.Debug("We are the primary client!")
			break
		} else {
			return fmt.Errorf("we are not the primary client")
		}
	}
	// set pipeline config
	time.Sleep(defaultWait)
	if _, err := sw.p4RtC.SetFwdPipeFromBytes(sw.binBytes, sw.p4infoBytes, 0); err != nil {
		return err
	}
	sw.log.Debug("Setted forwarding pipe")
	//
	digestConfig := &p4_v1.DigestEntry_Config{
		MaxTimeoutNs: 0,
		MaxListSize:  1,
		AckTimeoutNs: time.Second.Nanoseconds() * 1000,
	}
	if err := sw.p4RtC.EnableDigest(digestName, digestConfig); err != nil {
		return fmt.Errorf("cannot enable digest %s", digestName)
	}
	sw.log.Debugf("Enabled digest %s", digestName)

	sw.errCh = make(chan error, 1)
	sw.addConfig()
	go sw.handleStreamMessages(conn)
	go sw.startRunner()

	sw.log.Debug("Switch configured")
	return nil
}

func (sw *GrpcSwitch) startRunner() {
	defer func() {
		close(sw.messageCh)
		sw.log.Info("Stopping")
	}()
	// handle ticker
	ticker := time.NewTicker(packetCheckRate)
	for {
		select {
		case <-ticker.C:
			//sw.readCounter()
		case err := <-sw.errCh:
			sw.log.Errorf("%v", err)
			go sw.reconnect()
			return
		case <-sw.ctx.Done():
			return
		}
	}
}

func (sw *GrpcSwitch) reconnect() {
	if sw.restarts >= maxRetry {
		sw.log.Errorf("Max retry attempt, killing")
		return
	}
	sw.restarts++
	sw.log.Infof("Reconnect attempt n. %d", sw.restarts)
	if err := sw.runSwitch(); err != nil {
		sw.log.Errorf("%v", err)
		time.Sleep(reconnectTimeout)
		sw.reconnect()
	} else {
		// reset retries
		sw.restarts = 0
	}
}

func (sw *GrpcSwitch) handleDigest(digestList *p4_v1.DigestList) {
	for _, digestData := range digestList.Data {
		s := digestData.GetStruct()
		flow := s.Members[0].GetBitstring()
		flowOpp := s.Members[1].GetBitstring()
		tmp := s.Members[2].GetBitstring()
		var zero_byte byte = 0
		var treshold uint16
		if len(tmp) == 1 {
			treshold = binary.BigEndian.Uint16([]byte{zero_byte, tmp[0]})
		} else {
			treshold = binary.BigEndian.Uint16(tmp)
		}
		sw.log.WithFields(log.Fields{
			"flow":     flow,
			"flow opp": flowOpp,
			"treshold": treshold,
		}).Debug()
	}
	if err := sw.p4RtC.AckDigestList(digestList); err != nil {
		sw.errCh <- err
	}
	sw.log.Trace("Ack digest list")
}

// not used so no error handling
func (sw *GrpcSwitch) handleStreamMessages(conn *grpc.ClientConn) {
	defer conn.Close()
	for message := range sw.messageCh {
		switch m := message.Update.(type) {
		case *p4_v1.StreamMessageResponse_Packet:
			sw.log.Debug("Received Packetin")
		case *p4_v1.StreamMessageResponse_Digest:
			sw.log.Trace("Received DigestList")
			sw.handleDigest(m.Digest)
		case *p4_v1.StreamMessageResponse_IdleTimeoutNotification:
			sw.log.Debug("Received IdleTimeoutNotification")
		case *p4_v1.StreamMessageResponse_Error:
			sw.log.Trace("Received StreamError")
			sw.errCh <- fmt.Errorf("StreamError: %v", m.Error)
		default:
			sw.log.Debug("Received unknown stream message")
		}
	}
	sw.log.Trace("Closed message channel")
	time.Sleep(defaultWait)
}

func (sw *GrpcSwitch) readCounter() {
	sw.log.Debug("Reading counter")
	for port := 1; port <= sw.ports; port++ {
		lFields := log.WithFields(log.Fields{"ID": sw.id, "Port": port})
		// read counter
		counter, err := sw.p4RtC.ReadCounterEntry(packetCounter, int64(port))
		if err != nil {
			sw.errCh <- err
			return
		}
		// log counter
		if counter.GetPacketCount() > packetCountWarn {
			lFields.Warnf("Packet count %d", counter.GetPacketCount())
		} else {
			lFields.Debugf("Packet count %d", counter.GetPacketCount())
		}
		// reset counter
		if err = sw.p4RtC.ModifyCounterEntry(
			packetCounter,
			int64(port),
			&p4_v1.CounterData{PacketCount: 0},
		); err != nil {
			sw.errCh <- err
			return
		}
	}
}

func (sw *GrpcSwitch) addTableEntry(ip []byte, mac []byte, port []byte) {
	entry := sw.p4RtC.NewTableEntry(
		"MyIngress.ipv4_lpm",
		[]client.MatchInterface{&client.LpmMatch{
			Value: ip,
			PLen:  32,
		}},
		sw.p4RtC.NewTableActionDirect("MyIngress.ipv4_forward", [][]byte{mac, port}),
		nil,
	)
	if err := sw.p4RtC.InsertTableEntry(entry); err != nil {
		sw.errCh <- err
		return
	}
	sw.log.Debugf("Added table entry to device")
}

func (sw *GrpcSwitch) addConfig() {
	for _, link := range GetLinksBytes(sw.id) {
		sw.addTableEntry(link.ip, link.mac, link.port)
	}
}
