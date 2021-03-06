package gontpd

import (
	"context"
	"fmt"
	"log"
	"net"
	"syscall"
	"time"

	"github.com/rainycape/geoip"
	"golang.org/x/sys/unix"
)

// minimum headway time is 2 seconds, https://www.eecis.udel.edu/~mills/ntp/html/rate.html
const limit = 2

func (d *NTPd) listen() {

	var geodb *geoip.GeoIP
	if d.cfg.GeoDB != "" {
		var err error
		geodb, err = geoip.Open(d.cfg.GeoDB)
		if err != nil {
			log.Println(err)
		}
	}

	for j := 0; j < d.cfg.ConnNum; j++ {
		conn, err := d.makeConn()
		if err != nil {
			log.Fatal(err)
		}
		for i := 0; i < d.cfg.WorkerNum; i++ {
			id := fmt.Sprintf("%d:%d", j, i)
			var ws *workerStat
			if d.cfg.Metric != "" {
				ws = newWorkerStat(id)
			}

			w := worker{
				id, newLRU(d.cfg.RateSize),
				conn, ws, d,
				geodb,
			}
			go w.Work()
		}
	}
}

type worker struct {
	id    string
	lru   *lru
	conn  *net.UDPConn
	stat  *workerStat
	d     *NTPd
	geoDB *geoip.GeoIP
}

func (d *NTPd) makeConn() (conn *net.UDPConn, err error) {

	var operr error

	cfgFn := func(network, address string, conn syscall.RawConn) (err error) {

		fn := func(fd uintptr) {
			operr = syscall.SetsockoptInt(int(fd),
				syscall.SOL_SOCKET,
				unix.SO_REUSEPORT, 1)
			if operr != nil {
				return
			}
			/*
				TODO
				rerr := syscall.SetsockoptInt(int(fd),
					syscall.SOL_SOCKET,
					unix.SO_ATTACH_REUSEPORT_EBPF, 1)
				if rerr != nil {
					log.Println("set attach reuseport failed:", rerr, "but continue...")
				}
			*/
		}

		if err = conn.Control(fn); err != nil {
			return err
		}

		err = operr
		return
	}
	lc := net.ListenConfig{Control: cfgFn}
	lp, err := lc.ListenPacket(context.Background(), "udp", d.cfg.Listen)
	if err != nil {
		return
	}
	conn = lp.(*net.UDPConn)
	return
}

func (w *worker) Work() {
	var (
		receiveTime time.Time
		remoteAddr  *net.UDPAddr

		err      error
		lastUnix int64
		n        int
		ok       bool
	)
	p := make([]byte, 48)
	oob := make([]byte, 1)

	log.Printf("worker %s started", w.id)

	for {
		n, _, _, remoteAddr, err = w.conn.ReadMsgUDP(p, oob)
		if err != nil {
			return
		}

		receiveTime = time.Now()
		if n < 48 {
			if debug {
				log.Printf("worker: %s get small packet %d",
					remoteAddr.String(), n)
			}
			if w.stat != nil {
				w.stat.Malform.Inc()
			}
			continue
		}

		if w.d.dropTable.contains(remoteAddr.IP) {
			if debug {
				log.Printf("worker: %s drop packet %d",
					remoteAddr.String(), n)
			}
			if w.stat != nil {
				w.stat.ACL.Inc()
			}
			continue
		}

		// BCE
		_ = p[47]

		if w.d.cfg.RateSize > 0 {

			lastUnix, ok = w.lru.Get(remoteAddr.IP)

			if ok && receiveTime.Unix()-lastUnix < limit {

				if !w.d.cfg.RateDrop {
					w.sendError(p, remoteAddr, rateKoD)
				}
				if w.stat != nil {
					w.stat.Rate.Inc()
				}
				continue
			}

			w.lru.Add(remoteAddr.IP, receiveTime.Unix())
		}

		// GetMode

		switch p[liVnModePos] &^ 0xf8 {
		case modeSymmetricActive:
			w.sendError(p, remoteAddr, acstKoD)
		case modeReserved:
			fallthrough
		case modeClient:
			copy(p[0:originTimeStamp], w.d.template)
			copy(p[originTimeStamp:originTimeStamp+8],
				p[transmitTimeStamp:transmitTimeStamp+8])
			setUint64(p, receiveTimeStamp, toNtpTime(receiveTime))
			setUint64(p, transmitTimeStamp, toNtpTime(time.Now()))
			_, err = w.conn.WriteToUDP(p, remoteAddr)
			if err != nil && debug {
				log.Printf("worker: %s write failed. %s", remoteAddr.String(), err)
			}
			if w.stat == nil {
				continue
			}
			w.stat.Req.Inc()
			if w.stat.GeoDB != nil {
				w.logIP(remoteAddr)
			}
		default:
			if debug {
				log.Printf("%s not support client request mode:%x",
					remoteAddr.String(), p[liVnModePos]&^0xf8)
			}
			if w.stat != nil {
				w.stat.Unknown.Inc()
			}
		}
	}
}

func (w *worker) sendError(p []byte, raddr *net.UDPAddr, err uint32) {
	// avoid spoof
	copy(p[0:originTimeStamp], w.d.template)
	copy(p[originTimeStamp:originTimeStamp+8],
		p[transmitTimeStamp:transmitTimeStamp+8])
	setUint8(p, stratumPos, 0)
	setUint32(p, referIDPos, err)
	w.conn.WriteToUDP(p, raddr)
}

func (w *worker) logIP(raddr *net.UDPAddr) {
	country, err := w.stat.GeoDB.LookupIP(raddr.IP)
	if err != nil {
		return
	}
	if country.Country == nil {
		return
	}
	w.stat.CCReq.WithLabelValues(country.Country.Code).Inc()
}
