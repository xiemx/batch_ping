package ping

import (
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"sync"
	"time"

	"golang.org/x/net/icmp"
	"golang.org/x/net/ipv4"
	"golang.org/x/net/ipv6"
)

// BatchPinger is Pinger manager
type BatchPinger struct {
	// pingers []*Pinger
	done chan bool

	//mapSeqPinger is seqId pinger map
	mapSeqPinger map[int]*Pinger

	// interval is the wait time between each packet send. Default is 1s.
	interval time.Duration

	// Timeout specifies a timeout before ping exits, regardless of how many
	// packets have been received.
	timeout time.Duration

	// Count tells pinger to stop after sending (and receiving) Count echo
	// packets. If this option is not specified, pinger will operate until
	// interrupted.

	//count is ping num for every addr
	count int

	//sendCount is the num has send
	sendCount int

	//source is source ip, can use this ip listen
	source string

	//network  is network mode, may be ip or udp, and ip is privileged
	network string

	//id is the process id, should drop the pkg of other process
	id int

	//conn4 is ipv4 icmp PacketConn
	conn4 *icmp.PacketConn

	//conn6 is ipv6 icmp PacketConn
	conn6 *icmp.PacketConn

	//addrs is all addr
	addrs []string

	//debug model will print log
	debug bool

	// OnFinish can be called when Pinger exits
	OnFinish func(map[string]*Statistics)
}

//NewBatchPinger returns a new Pinger struct pointer, interval is default 1s, count default 5, count should not more than 65535
func NewBatchPinger(addrs []string, privileged bool) (batachPinger *BatchPinger, err error) {

	// addrs can not more than 65535
	if len(addrs) > 0xffff {
		return nil, errors.New("addr can not more than 65535")
	}
	var network string
	if privileged {
		network = "ip"
	} else {
		network = "udp"
	}

	batachPinger = &BatchPinger{
		interval:     time.Second,
		timeout:      time.Second * 100000,
		count:        5,
		network:      network,
		id:           getPId(),
		mapSeqPinger: make(map[int]*Pinger),
		done:         make(chan bool),
		addrs:        addrs,
	}

	return batachPinger, nil
}

// SetDebug will fmt debug log
func (bp *BatchPinger) SetDebug(debug bool) {
	bp.debug = debug
}

// SetSource set source ip
func (bp *BatchPinger) SetSource(source string) {
	bp.source = source
}

// SetCount set ping count per addr
func (bp *BatchPinger) SetCount(count int) {
	bp.count = count
}

// SetInterval set ping interval
func (bp *BatchPinger) SetInterval(interval time.Duration) {
	bp.interval = interval
}

// SetTimeout Timeout specifies a timeout before ping exits, regardless of how many packets have been received.
func (bp *BatchPinger) SetTimeout(timeout time.Duration) {
	bp.timeout = timeout
}

// getPId get process id
func getPId() int {
	return os.Getpid()
}

// Run will multi-ping addrs
func (bp *BatchPinger) Run() (err error) {
	if bp.conn4, err = icmp.ListenPacket(ipv4Proto[bp.network], bp.source); err != nil {
		return err
	}
	if bp.conn6, err = icmp.ListenPacket(ipv6Proto[bp.network], bp.source); err != nil {
		return err
	}
	bp.conn4.IPv4PacketConn().SetControlMessage(ipv4.FlagTTL, true)
	bp.conn6.IPv6PacketConn().SetControlMessage(ipv6.FlagHopLimit, true)

	var seqID int
	for _, addr := range bp.addrs {
		seqID++
		pinger, err := NewPinger(addr, bp.id, seqID, bp.network)
		if err != nil {
			return err
		}
		bp.mapSeqPinger[seqID] = pinger
		pinger.SetConns(bp.conn4, bp.conn6)
	}

	if bp.debug {
		log.Printf("[debug] pid %d \n", bp.id)
	}

	defer bp.conn4.Close()
	defer bp.conn6.Close()

	var wg sync.WaitGroup
	wg.Add(3)
	go bp.recvIpv4(&wg)
	go bp.recvIpv6(&wg)
	go bp.sendICMP(&wg)
	wg.Wait()
	return nil
}

func (bp *BatchPinger) recvIpv4(wg *sync.WaitGroup) {
	defer wg.Done()
	var ttl int

	for {
		select {
		case <-bp.done:
			return
		default:
			bytes := make([]byte, 512)
			bp.conn4.SetReadDeadline(time.Now().Add(time.Millisecond * 100))
			n, cm, _, err := bp.conn4.IPv4PacketConn().ReadFrom(bytes)
			if cm != nil {
				ttl = cm.TTL
			}

			if err != nil {
				if neterr, ok := err.(*net.OpError); ok {
					if neterr.Timeout() {
						// Read timeout
						continue
					} else {
						if bp.debug {
							log.Printf("read err %s ", err)
						}
						return
					}
				}
			}
			recvPkg := &packet{bytes: bytes, nbytes: n, ttl: ttl, proto: protoIpv4}
			if bp.debug {
				log.Printf("recv pkg %v \n", recvPkg)
			}
			bp.processPacket(recvPkg)
		}
	}
}

func (bp *BatchPinger) recvIpv6(wg *sync.WaitGroup) {
	defer wg.Done()
	var ttl int
	for {
		select {
		case <-bp.done:
			return
		default:
			bytes := make([]byte, 512)
			bp.conn6.SetReadDeadline(time.Now().Add(time.Millisecond * 100))
			n, cm, _, err := bp.conn6.IPv6PacketConn().ReadFrom(bytes)
			if cm != nil {
				ttl = cm.HopLimit
			}
			if err != nil {
				if neterr, ok := err.(*net.OpError); ok {
					if neterr.Timeout() {
						// Read timeout
						continue
					}
				}
			}
			recvPkg := &packet{bytes: bytes, nbytes: n, ttl: ttl, proto: protoIpv6}
			if bp.debug {
				log.Printf("recv pkg %v \n", recvPkg)
			}
			bp.processPacket(recvPkg)
		}

	}
}

func (bp *BatchPinger) sendICMP(wg *sync.WaitGroup) {
	defer wg.Done()
	timeout := time.NewTicker(bp.timeout)
	interval := time.NewTicker(bp.interval)

	for {
		select {
		case <-bp.done:
			return

		case <-timeout.C:
			close(bp.done)
			return

		case <-interval.C:
			bp.batchSendICMP()
			bp.sendCount++
			if bp.sendCount >= bp.count {
				time.Sleep(bp.interval)
				close(bp.done)
				if bp.debug {
					log.Printf("send end sendcout %d, count %d \n", bp.sendCount, bp.count)
				}

				return
			}
		}
	}
}

// batchSendICMP let all addr send pkg once
func (bp *BatchPinger) batchSendICMP() {
	for _, pinger := range bp.mapSeqPinger {
		pinger.SendICMP()
		pinger.PacketsSent++
	}
}

func (bp *BatchPinger) processPacket(recv *packet) error {
	receivedAt := time.Now()
	var proto int
	if recv.proto == protoIpv4 {
		proto = protocolICMP
	} else {
		proto = protocolIPv6ICMP
	}

	var m *icmp.Message
	var err error

	if m, err = icmp.ParseMessage(proto, recv.bytes); err != nil {
		return fmt.Errorf("error parsing icmp message: %s", err.Error())
	}

	if m.Type != ipv4.ICMPTypeEchoReply && m.Type != ipv6.ICMPTypeEchoReply {
		// Not an echo reply, ignore it
		if bp.debug {
			log.Printf("pkg drop %v \n", m)
		}
		return nil
	}

	switch pkt := m.Body.(type) {
	case *icmp.Echo:
		// If we are privileged, we can match icmp.ID
		if pkt.ID != bp.id {
			if bp.debug {
				log.Printf("drop pkg %+v id %v\n", pkt, bp.id)
			}
			return nil
		}

		if len(pkt.Data) < timeSliceLength+trackerLength {
			return fmt.Errorf("insufficient data received; got: %d %v",
				len(pkt.Data), pkt.Data)
		}

		timestamp := bytesToTime(pkt.Data[:timeSliceLength])

		if pinger, ok := bp.mapSeqPinger[pkt.Seq]; ok {
			pinger.PacketsRecv++
			pinger.rtts = append(pinger.rtts, receivedAt.Sub(timestamp))
		}

	default:
		// Very bad, not sure how this can happen
		return fmt.Errorf("invalid ICMP echo reply; type: '%T', '%v'", pkt, pkt)
	}

	return nil

}

// Statistics is all addr data Statistic
func (bp *BatchPinger) Statistics() map[string]*Statistics {
	stMap := map[string]*Statistics{}
	for _, pinger := range bp.mapSeqPinger {
		x := pinger.Statistics()
		stMap[pinger.addr] = x
	}
	return stMap
}

// Finish will call OnFinish
func (bp *BatchPinger) Finish() {
	handler := bp.OnFinish
	if bp.OnFinish != nil {
		handler(bp.Statistics())
	}
}
