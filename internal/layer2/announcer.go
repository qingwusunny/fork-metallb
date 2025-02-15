// SPDX-License-Identifier:Apache-2.0

package layer2

import (
	"io/ioutil"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/go-kit/log"
	"github.com/go-kit/log/level"
)

// Announce is used to "announce" new IPs mapped to the node's MAC address.
type Announce struct {
	logger log.Logger

	sync.RWMutex
	arps     map[int]*arpResponder
	ndps     map[int]*ndpResponder
	ips      map[string][]net.IP // svcName -> IPs
	ipRefcnt map[string]int      // ip.String() -> number of uses

	// This channel can block - do not write to it while holding the mutex
	// to avoid deadlocking.
	spamCh chan net.IP
}

// New returns an initialized Announce.
func New(l log.Logger) (*Announce, error) {
	ret := &Announce{
		logger:   l,
		arps:     map[int]*arpResponder{},
		ndps:     map[int]*ndpResponder{},
		ips:      map[string][]net.IP{},
		ipRefcnt: map[string]int{},
		spamCh:   make(chan net.IP, 1024),
	}
	go ret.interfaceScan()
	go ret.spamLoop()

	return ret, nil
}

func (a *Announce) interfaceScan() {
	for {
		a.updateInterfaces()
		time.Sleep(10 * time.Second)
	}
}

func (a *Announce) updateInterfaces() {
	ifs, err := net.Interfaces()
	if err != nil {
		level.Error(a.logger).Log("op", "getInterfaces", "error", err, "msg", "couldn't list interfaces")
		return
	}

	a.Lock()
	defer a.Unlock()

	keepARP, keepNDP := map[int]bool{}, map[int]bool{}
	for _, intf := range ifs {
		ifi := intf
		l := log.With(a.logger, "interface", ifi.Name)
		addrs, err := ifi.Addrs()
		if err != nil {
			level.Error(l).Log("op", "getAddresses", "error", err, "msg", "couldn't get addresses for interface")
			return
		}

		if ifi.Flags&net.FlagUp == 0 {
			continue
		}
		if _, err = os.Stat("/sys/class/net/" + ifi.Name + "/master"); !os.IsNotExist(err) {
			continue
		}
		f, err := ioutil.ReadFile("/sys/class/net/" + ifi.Name + "/flags")
		if err == nil {
			flags, _ := strconv.ParseUint(string(f)[:len(string(f))-1], 0, 32)
			// NOARP flag
			if flags&0x80 != 0 {
				continue
			}
		}

		for _, a := range addrs {
			ipaddr, ok := a.(*net.IPNet)
			if !ok {
				continue
			}
			if ipaddr.IP.To4() != nil && (ifi.Flags&net.FlagBroadcast) != 0 {
				keepARP[ifi.Index] = true
			}
			if ipaddr.IP.IsLinkLocalUnicast() {
				keepNDP[ifi.Index] = true
			}
		}

		if keepARP[ifi.Index] && a.arps[ifi.Index] == nil {
			resp, err := newARPResponder(a.logger, &ifi, a.shouldAnnounce)
			if err != nil {
				level.Error(l).Log("op", "createARPResponder", "error", err, "msg", "failed to create ARP responder")
				return
			}
			a.arps[ifi.Index] = resp
			level.Info(l).Log("event", "createARPResponder", "msg", "created ARP responder for interface")
		}
		if keepNDP[ifi.Index] && a.ndps[ifi.Index] == nil {
			resp, err := newNDPResponder(a.logger, &ifi, a.shouldAnnounce)
			if err != nil {
				level.Error(l).Log("op", "createNDPResponder", "error", err, "msg", "failed to create NDP responder")
				return
			}
			a.ndps[ifi.Index] = resp
			level.Info(l).Log("event", "createNDPResponder", "msg", "created NDP responder for interface")
		}
	}

	for i, client := range a.arps {
		if !keepARP[i] {
			client.Close()
			delete(a.arps, i)
			level.Info(a.logger).Log("interface", client.Interface(), "event", "deleteARPResponder", "msg", "deleted ARP responder for interface")
		}
	}
	for i, client := range a.ndps {
		if !keepNDP[i] {
			client.Close()
			delete(a.ndps, i)
			level.Info(a.logger).Log("interface", client.Interface(), "event", "deleteNDPResponder", "msg", "deleted NDP responder for interface")
		}
	}
}

func (a *Announce) spamLoop() {
	// Map IP to spam stop time.
	m := map[string]time.Time{}
	// We can't create a stopped ticker, so create one with a big period to avoid ticking for nothing
	ticker := time.NewTicker(time.Hour)
	ticker.Stop()
	for {
		select {
		case ip := <-a.spamCh:
			if len(m) == 0 {
				// See https://github.com/metallb/metallb/issues/172 for the 1100 choice.
				ticker.Reset(1100 * time.Millisecond)
			}
			ipStr := ip.String()
			_, ok := m[ipStr]
			// Set spam stop time to 5 seconds from now.
			m[ipStr] = time.Now().Add(5 * time.Second)
			if !ok {
				// Spam right away to avoid waiting up to 1100 milliseconds even if
				// it means we call gratuitous() twice in a row in a short amount of time.
				a.gratuitous(ip)
			}
		case now := <-ticker.C:
			for ipStr, until := range m {
				if now.After(until) {
					// We have spammed enough - remove the IP from the map.
					delete(m, ipStr)
				} else {
					a.gratuitous(net.ParseIP(ipStr))
				}
			}
			if len(m) == 0 {
				ticker.Stop()
			}
		}
	}
}

func (a *Announce) doSpam(ip net.IP) {
	a.spamCh <- ip
}

func (a *Announce) gratuitous(ip net.IP) {
	a.RLock()
	defer a.RUnlock()

	if a.ipRefcnt[ip.String()] <= 0 {
		// We've lost control of the IP, someone else is
		// doing announcements.
		return
	}

	if ip.To4() != nil {
		for _, client := range a.arps {
			if err := client.Gratuitous(ip); err != nil {
				level.Error(a.logger).Log("op", "gratuitousAnnounce", "error", err, "ip", ip, "msg", "failed to make gratuitous ARP announcement")
			}
		}
	} else {
		for _, client := range a.ndps {
			if err := client.Gratuitous(ip); err != nil {
				level.Error(a.logger).Log("op", "gratuitousAnnounce", "error", err, "ip", ip, "msg", "failed to make gratuitous NDP announcement")
			}
		}
	}
}

func (a *Announce) shouldAnnounce(ip net.IP) dropReason {
	a.RLock()
	defer a.RUnlock()
	for _, ips := range a.ips {
		for _, i := range ips {
			if i.Equal(ip) {
				return dropReasonNone
			}
		}
	}
	return dropReasonAnnounceIP
}

// SetBalancer adds ip to the set of announced addresses.
func (a *Announce) SetBalancer(name string, ip net.IP) {
	// Call doSpam at the end of the function without holding the lock
	defer a.doSpam(ip)
	a.Lock()
	defer a.Unlock()

	// Kubernetes may inform us that we should advertise this address multiple
	// times, so just no-op any subsequent requests.
	if ips, ok := a.ips[name]; ok {
		for i, ip := range ips {
			if ip.Equal(a.ips[name][i]) {
				continue
			}
		}
	}

	a.ips[name] = append(a.ips[name], ip)

	a.ipRefcnt[ip.String()]++
	if a.ipRefcnt[ip.String()] > 1 {
		// Multiple services are using this IP, so there's nothing
		// else to do right now.
		return
	}

	for _, client := range a.ndps {
		if err := client.Watch(ip); err != nil {
			level.Error(a.logger).Log("op", "watchMulticastGroup", "error", err, "ip", ip, "msg", "failed to watch NDP multicast group for IP, NDP responder will not respond to requests for this address")
		}
	}
}

// DeleteBalancer deletes an address from the set of addresses we should announce.
func (a *Announce) DeleteBalancer(name string) {
	a.Lock()
	defer a.Unlock()

	ips, ok := a.ips[name]
	if !ok {
		return
	}
	delete(a.ips, name)
	for _, ip := range ips {
		a.ipRefcnt[ip.String()]--
		if a.ipRefcnt[ip.String()] > 0 {
			// Another service is still using this IP, don't touch any
			// more things.
			return
		}

		for _, client := range a.ndps {
			if err := client.Unwatch(ip); err != nil {
				level.Error(a.logger).Log("op", "unwatchMulticastGroup", "error", err, "ip", ip, "msg", "failed to unwatch NDP multicast group for IP")
			}
		}
	}

}

// AnnounceName returns true when we have an announcement under name.
func (a *Announce) AnnounceName(name string) bool {
	a.RLock()
	defer a.RUnlock()
	_, ok := a.ips[name]
	return ok
}

// dropReason is the reason why a layer2 protocol packet was not
// responded to.
type dropReason int

// Various reasons why a packet was dropped.
const (
	dropReasonNone dropReason = iota
	dropReasonClosed
	dropReasonError
	dropReasonARPReply
	dropReasonMessageType
	dropReasonNoSourceLL
	dropReasonEthernetDestination
	dropReasonAnnounceIP
)
