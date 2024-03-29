package vxlan

import (
	"fmt"
	"net"
	"os"
	"syscall"
	"time"

	log "github.com/coreos/flannel/Godeps/_workspace/src/github.com/golang/glog"
	"github.com/coreos/flannel/Godeps/_workspace/src/github.com/vishvananda/netlink"
	"github.com/coreos/flannel/Godeps/_workspace/src/github.com/vishvananda/netlink/nl"

	"github.com/coreos/flannel/pkg/ip"
)

type vxlanDeviceAttrs struct {
	vni       uint32
	name      string
	vtepIndex int
	vtepAddr  net.IP
	vtepPort  int
}

type vxlanDevice struct {
	link *netlink.Vxlan
}

func sysctlSet(path, value string) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()

	_, err = f.Write([]byte(value))
	return err
}

func newVXLANDevice(devAttrs *vxlanDeviceAttrs) (*vxlanDevice, error) {
	link := &netlink.Vxlan{
		LinkAttrs: netlink.LinkAttrs{
			Name: devAttrs.name,
		},
		VxlanId:      int(devAttrs.vni),
		VtepDevIndex: devAttrs.vtepIndex,
		SrcAddr:      devAttrs.vtepAddr,
		Port:         devAttrs.vtepPort,
		Learning:     true,
		Proxy:        true,
		L2miss:       true,
	}

	link, err := ensureLink(link)
	if err != nil {
		return nil, err
	}

	// this enables ARP requests being sent to userspace via netlink
	sysctlPath := fmt.Sprintf("/proc/sys/net/ipv4/neigh/%s/app_solicit", devAttrs.name)
	sysctlSet(sysctlPath, "3")

	return &vxlanDevice{
		link: link,
	}, nil
}

func ensureLink(vxlan *netlink.Vxlan) (*netlink.Vxlan, error) {
	err := netlink.LinkAdd(vxlan)
	if err != nil {
		if err == syscall.EEXIST {
			// it's ok if the device already exists as long as config is similar
			existing, err := netlink.LinkByName(vxlan.Name)
			if err != nil {
				return nil, err
			}
			if incompat := vxlanLinksIncompat(vxlan, existing); incompat != "" {
				log.Warningf("%q already exists with incompatable configuration: %v; recreating device", vxlan.Name, incompat)

				// delete existing
				if err = netlink.LinkDel(existing); err != nil {
					return nil, fmt.Errorf("failed to delete interface: %v", err)
				}

				// create new
				if err = netlink.LinkAdd(vxlan); err != nil {
					return nil, fmt.Errorf("failed to create vxlan interface: %v", err)
				}
			} else {
				return existing.(*netlink.Vxlan), nil
			}
		} else {
			return nil, err
		}
	}

	ifindex := vxlan.Index
	link, err := netlink.LinkByIndex(vxlan.Index)
	if err != nil {
		return nil, fmt.Errorf("can't locate created vxlan device with index %v", ifindex)
	}
	var ok bool
	if vxlan, ok = link.(*netlink.Vxlan); !ok {
		return nil, fmt.Errorf("created vxlan device with index %v is not vxlan", ifindex)
	}

	return vxlan, nil
}

func (dev *vxlanDevice) Configure(ipn ip.IP4Net) error {
	setAddr4(dev.link, ipn.ToIPNet())

	if err := netlink.LinkSetUp(dev.link); err != nil {
		return fmt.Errorf("failed to set interface %s to UP state: %s", dev.link.Attrs().Name, err)
	}

	// explicitly add a route since there might be a route for a subnet already
	// installed by Docker and then it won't get auto added
	route := netlink.Route{
		LinkIndex: dev.link.Attrs().Index,
		Scope:     netlink.SCOPE_UNIVERSE,
		Dst:       ipn.Network().ToIPNet(),
	}
	if err := netlink.RouteAdd(&route); err != nil && err != syscall.EEXIST {
		return fmt.Errorf("failed to add route (%s -> %s): %v", ipn.Network().String(), dev.link.Attrs().Name, err)
	}

	return nil
}

func (dev *vxlanDevice) Destroy() {
	netlink.LinkDel(dev.link)
}

func (dev *vxlanDevice) MACAddr() net.HardwareAddr {
	return dev.link.HardwareAddr
}

func (dev *vxlanDevice) MTU() int {
	return dev.link.MTU
}

func (dev *vxlanDevice) MonitorMisses(misses chan *netlink.Neigh) {
	nlsock, err := nl.Subscribe(syscall.NETLINK_ROUTE, syscall.RTNLGRP_NEIGH)
	if err != nil {
		log.Error("Failed to subscribe to netlink RTNLGRP_NEIGH messages")
		return
	}

	for {
		msgs, err := nlsock.Recieve()
		if err != nil {
			log.Errorf("Failed to receive from netlink: %v ", err)

			// wait 1 sec before retrying but honor the cancel channel
			time.Sleep(1*time.Second)
			continue
		}

		for _, msg := range msgs {
			dev.processNeighMsg(msg, misses)
		}
	}
}

func isNeighResolving(state int) bool {
	return (state & (netlink.NUD_INCOMPLETE | netlink.NUD_STALE | netlink.NUD_DELAY | netlink.NUD_PROBE)) != 0
}

func (dev *vxlanDevice) processNeighMsg(msg syscall.NetlinkMessage, misses chan *netlink.Neigh) {
	neigh, err := netlink.NeighDeserialize(msg.Data)
	if err != nil {
		log.Error("Failed to deserialize netlink ndmsg: %v", err)
		return
	}

	if int(neigh.LinkIndex) != dev.link.Index {
		return
	}

	if msg.Header.Type != syscall.RTM_GETNEIGH && msg.Header.Type != syscall.RTM_NEWNEIGH {
		return
	}

	if !isNeighResolving(neigh.State) {
		// misses come with NUD_STALE bit set
		return
	}

	misses <- neigh
}

func (dev *vxlanDevice) AddL2(mac net.HardwareAddr, vtep net.IP) error {
	neigh := netlink.Neigh{
		LinkIndex:    dev.link.Index,
		State:        netlink.NUD_REACHABLE,
		Family:       syscall.AF_BRIDGE,
		Flags:        netlink.NTF_SELF,
		IP:           vtep,
		HardwareAddr: mac,
	}

	log.Infof("calling NeighAdd: %v, %v", vtep, mac)
	return netlink.NeighAdd(&neigh)
}

func (dev *vxlanDevice) DelL2(mac net.HardwareAddr, vtep net.IP) error {
	neigh := netlink.Neigh{
		LinkIndex:    dev.link.Index,
		Family:       syscall.AF_BRIDGE,
		Flags:        netlink.NTF_SELF,
		IP:           vtep,
		HardwareAddr: mac,
	}

	log.Infof("calling NeighDel: %v, %v", vtep, mac)
	return netlink.NeighDel(&neigh)
}

func (dev *vxlanDevice) AddL3(ip net.IP, mac net.HardwareAddr) error {
	neigh := netlink.Neigh{
		LinkIndex:    dev.link.Index,
		State:        netlink.NUD_REACHABLE,
		Type:         syscall.RTN_UNICAST,
		IP:           ip,
		HardwareAddr: mac,
	}

	log.Infof("calling NeighSet: %v, %v", ip, mac)
	return netlink.NeighSet(&neigh)
}

func vxlanLinksIncompat(l1, l2 netlink.Link) string {
	if l1.Type() != l2.Type() {
		return fmt.Sprintf("link type: %v vs %v", l1.Type(), l2.Type())
	}

	v1 := l1.(*netlink.Vxlan)
	v2 := l2.(*netlink.Vxlan)

	if v1.VxlanId != v2.VxlanId {
		return fmt.Sprintf("vni: %v vs %v", v1.VxlanId, v2.VxlanId)
	}

	if v1.VtepDevIndex > 0 && v2.VtepDevIndex > 0 && v1.VtepDevIndex != v2.VtepDevIndex {
		return fmt.Sprintf("vtep (external) interface: %v vs %v", v1.VtepDevIndex, v2.VtepDevIndex)
	}

	if len(v1.SrcAddr) > 0 && len(v2.SrcAddr) > 0 && !v1.SrcAddr.Equal(v2.SrcAddr) {
		return fmt.Sprintf("vtep (external) IP: %v vs %v", v1.SrcAddr, v2.SrcAddr)
	}

	if len(v1.Group) > 0 && len(v2.Group) > 0 && !v1.Group.Equal(v2.Group) {
		return fmt.Sprintf("group address: %v vs %v", v1.Group, v2.Group)
	}

	if v1.L2miss != v2.L2miss {
		return fmt.Sprintf("l2miss: %v vs %v", v1.L2miss, v2.L2miss)
	}

	if v1.Port > 0 && v2.Port > 0 && v1.Port != v2.Port {
		return fmt.Sprintf("port: %v vs %v", v1.Port, v2.Port)
	}

	return ""
}

// sets IP4 addr on link removing any existing ones first
func setAddr4(link *netlink.Vxlan, ipn *net.IPNet) error {
	addrs, err := netlink.AddrList(link, syscall.AF_INET)
	if err != nil {
		return err
	}

	for _, addr := range addrs {
		if err = netlink.AddrDel(link, &addr); err != nil {
			return fmt.Errorf("failed to delete IPv4 addr %s from %s", addr.String(), link.Attrs().Name)
		}
	}

	addr := netlink.Addr{ ipn, "" }
	if err = netlink.AddrAdd(link, &addr); err != nil {
		return fmt.Errorf("failed to add IP address %s to %s: %s", ipn.String(), link.Attrs().Name, err)
	}

	return nil
}
