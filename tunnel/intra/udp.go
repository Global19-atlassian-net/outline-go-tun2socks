// Copyright 2019 The Outline Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

// Derived from go-tun2socks's "direct" handler under the Apache 2.0 license.

package intra

import (
	"errors"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/eycorsican/go-tun2socks/common/log"
	"github.com/eycorsican/go-tun2socks/core"
)

// Summary of a non-DNS UDP association, reported when it is discarded.
type UDPSocketSummary struct {
	UploadBytes   int64 // Amount uploaded (bytes)
	DownloadBytes int64 // Amount downloaded (bytes)
	Duration      int32 // How long the socket was open (seconds)
}

type UDPListener interface {
	OnUDPSocketClosed(*UDPSocketSummary)
}

type tracker struct {
	conn     *net.UDPConn
	start    time.Time
	upload   int64 // bytes
	download int64 // bytes
	// Parameters used to implement the single-query socket optimization:
	complex bool   // True if the socket is not a oneshot DNS query.
	queryid uint16 // The DNS query ID for this socket, if there is one.
}

func makeTracker(conn *net.UDPConn) *tracker {
	return &tracker{conn, time.Now(), 0, 0, false, 0}
}

type udpHandler struct {
	sync.Mutex

	timeout  time.Duration
	udpConns map[core.UDPConn]*tracker
	fakedns  net.UDPAddr
	truedns  net.UDPAddr
	dns      DNSTransport
	listener UDPListener
}

// NewUDPHandler makes a UDP handler with Intra-style DNS redirection:
// All packets are routed directly to their destination, except packets whose
// destination is `fakedns`.  Those packets are redirected to `truedns`.
// Similarly, packets arriving from `truedns` have the source address replaced
// with `fakedns`.
// TODO: Remove truedns once DOH is working well
func NewUDPHandler(fakedns, truedns net.UDPAddr, dns DNSTransport, timeout time.Duration, listener UDPListener) core.UDPConnHandler {
	return &udpHandler{
		timeout:  timeout,
		udpConns: make(map[core.UDPConn]*tracker, 8),
		fakedns:  fakedns,
		truedns:  truedns,
		dns:      dns,
		listener: listener,
	}
}

func queryid(data []byte) int32 {
	if len(data) < 2 {
		return -1
	}
	return 0xFFFF & ((int32(data[0]) << 8) | int32(data[1]))
}

func (h *udpHandler) fetchUDPInput(conn core.UDPConn, t *tracker) {
	buf := core.NewBytes(core.BufSize)

	defer func() {
		h.Close(conn)
		core.FreeBytes(buf)
	}()

	for {
		t.conn.SetDeadline(time.Now().Add(h.timeout))
		n, addr, err := t.conn.ReadFrom(buf)
		if err != nil {
			return
		}

		udpaddr := addr.(*net.UDPAddr)
		if udpaddr.IP.Equal(h.truedns.IP) && udpaddr.Port == h.truedns.Port {
			// Pretend that the reply was from the fake DNS server.
			udpaddr = &h.fakedns
			if n < 2 {
				// Very short packet, cannot possibly be DNS.
				t.complex = true
			} else {
				responseid := queryid(buf)
				if t.queryid != uint16(responseid) {
					// Something very strange is going on
					t.complex = true
				}
			}
		} else {
			// This socket has been used for non-DNS traffic.
			t.complex = true
		}
		t.download += int64(n)
		_, err = conn.WriteFrom(buf[:n], udpaddr)
		if err != nil {
			log.Warnf("failed to write UDP data to TUN")
			return
		}
		if !t.complex {
			// This socket has only been used for DNS traffic, and just got a response.
			// UDP DNS sockets are typically only used for one response.
			return
		}
	}
}

func (h *udpHandler) Connect(conn core.UDPConn, target *net.UDPAddr) error {
	bindAddr := &net.UDPAddr{IP: nil, Port: 0}
	pc, err := net.ListenUDP(bindAddr.Network(), bindAddr)
	if err != nil {
		log.Errorf("failed to bind udp address")
		return err
	}
	t := makeTracker(pc)
	h.Lock()
	h.udpConns[conn] = t
	h.Unlock()
	go h.fetchUDPInput(conn, t)
	log.Infof("new proxy connection for target: %s:%s", target.Network(), target.String())
	return nil
}

func (h *udpHandler) doDoh(conn core.UDPConn, data []byte) {
	resp, err := h.dns.Query(data)
	if err == nil {
		conn.WriteFrom(resp, &h.fakedns)
	}
	// TODO: Convert HTTP errors into DNS errors.
}

func (h *udpHandler) ReceiveTo(conn core.UDPConn, data []byte, addr *net.UDPAddr) error {
	h.Lock()
	t, ok1 := h.udpConns[conn]
	h.Unlock()

	if !ok1 {
		return fmt.Errorf("connection %v->%v does not exists", conn.LocalAddr(), addr)
	}

	if addr.IP.Equal(h.fakedns.IP) && addr.Port == h.fakedns.Port {
		if h.dns != nil {
			// Use DOH.
			dataCopy := append([]byte{}, data...)
			go h.doDoh(conn, dataCopy)
		} else {
			// Send the query to the real DNS server.
			addr = &h.truedns
			id := queryid(data)
			if id < 0 {
				t.complex = true
			} else if t.upload == 0 {
				t.queryid = uint16(id)
			} else if t.queryid != uint16(id) {
				t.complex = true
			}
		}
	} else {
		t.complex = true
	}
	t.upload += int64(len(data))
	_, err := t.conn.WriteTo(data, addr)
	if err != nil {
		log.Warnf("failed to forward UDP payload")
		return errors.New("failed to write UDP data")
	}
	return nil
}

func (h *udpHandler) Close(conn core.UDPConn) {
	conn.Close()

	h.Lock()
	defer h.Unlock()

	if t, ok := h.udpConns[conn]; ok {
		t.conn.Close()
		duration := int32(time.Since(t.start).Seconds())
		h.listener.OnUDPSocketClosed(&UDPSocketSummary{t.upload, t.download, duration})
		delete(h.udpConns, conn)
	}
}
