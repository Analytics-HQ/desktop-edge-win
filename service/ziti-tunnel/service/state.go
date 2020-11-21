/*
 * Copyright NetFoundry, Inc.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 * https://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 *
 */

package service

import (
	"bufio"
	"encoding/json"
	"fmt"
	"github.com/openziti/desktop-edge-win/service/cziti"
	"github.com/openziti/desktop-edge-win/service/ziti-tunnel/config"
	"github.com/openziti/desktop-edge-win/service/ziti-tunnel/constants"
	"github.com/openziti/desktop-edge-win/service/ziti-tunnel/dto"
	"golang.org/x/sys/windows/registry"
	"golang.zx2c4.com/wireguard/tun"
	"golang.zx2c4.com/wireguard/windows/tunnel/winipcfg"
	"io"
	"net"
	"os"
	"strings"
	"time"
)

type RuntimeState struct {
	state   *dto.TunnelStatus
	tun     *tun.Device
	tunName string
	ids     map[string]*Id
}

func (t *RuntimeState) RemoveByFingerprint(fingerprint string) {
	delete(t.ids, fingerprint)
}

func (t *RuntimeState) Find(fingerprint string) *Id {
	return t.ids[fingerprint]
}

func (t *RuntimeState) SaveState() {
	// overwrite file if it exists
	_ = os.MkdirAll(config.Path(), 0644)

	log.Debugf("backing up config")
	backup,err := backupConfig()
	if err != nil {
		log.Warnf("could not backup config file! %v", err)
	} else {
		log.Debugf("config file backed up to: %s", backup)
	}

	cfg, err := os.OpenFile(config.File(), os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err != nil {
		log.Panicf("An unexpected and unrecoverable error has occurred while %s: %v", "opening the config file", err)
	}

	w := bufio.NewWriter(bufio.NewWriter(cfg))
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(t.ToStatus())
	_ = w.Flush()

	err = cfg.Close()
	if err != nil {
		log.Panicf("An unexpected and unrecoverable error has occurred while %s: %v", "closing the config file", err)
	}
	log.Debug("state saved")
}

func backupConfig() (string, error) {
	original, err := os.Open(config.File())
	if err != nil {
		return "", err
	}
	defer original.Close()
	backup := config.File() + ".backup"
	new, err := os.Create(backup)
	if err != nil {
		return "", err
	}
	defer new.Close()

	_, err = io.Copy(new, original)
	if err != nil {
		return "", err
	}
	return backup, err
}

func (t *RuntimeState) ToStatus() dto.TunnelStatus {
	var uptime int64

	now := time.Now()
	tunStart := now.Sub(TunStarted)
	uptime = tunStart.Milliseconds()

	clean := dto.TunnelStatus{
		Active:         t.state.Active,
		Duration:       uptime,
		Identities:     make([]*dto.Identity, len(t.ids)),
		IpInfo:         t.state.IpInfo,
		LogLevel:       t.state.LogLevel,
		ServiceVersion: Version,
		TunIpv4:        t.state.TunIpv4,
		TunIpv4Mask:    t.state.TunIpv4Mask,
	}

	i := 0
	for _, id := range t.ids {
		cid := Clean(id)
		clean.Identities[i] = &cid
		i++
	}

	return clean
}

func (t *RuntimeState) ToMetrics() dto.TunnelStatus {
	clean := dto.TunnelStatus{
		Identities:     make([]*dto.Identity, len(t.ids)),
	}

	i := 0
	for _, id := range t.ids {
		AddMetrics(id)
		clean.Identities[i] = &dto.Identity{
			Name:              id.Name,
			FingerPrint:       id.FingerPrint,
			Metrics:           id.Metrics,
		}
		i++
	}

	return clean
}

func (t *RuntimeState) CreateTun(ipv4 string, ipv4mask int) (net.IP, error) {
	log.Infof("creating TUN device: %s", TunName)
	tunDevice, err := tun.CreateTUN(TunName, 64*1024 - 1)
	if err == nil {
		t.tun = &tunDevice
		tunName, err2 := tunDevice.Name()
		if err2 == nil {
			t.tunName = tunName
		}
	} else {
		return nil, fmt.Errorf("error creating TUN device: (%v)", err)
	}

	if name, err := tunDevice.Name(); err == nil {
		log.Debugf("created TUN device [%s]", name)
	} else {
		return nil, fmt.Errorf("error getting TUN name: (%v)", err)
	}

	nativeTunDevice := tunDevice.(*tun.NativeTun)
	luid := winipcfg.LUID(nativeTunDevice.LUID())

	if strings.TrimSpace(ipv4) == "" {
		log.Infof("ip not provided using default: %v", ipv4)
		ipv4 = constants.Ipv4ip
	}
	if ipv4mask < constants.Ipv4MaxMask {
		log.Warnf("provided mask is very large: %d.", ipv4mask)
	}
	ip, ipnet, err := net.ParseCIDR(fmt.Sprintf("%s/%d", ipv4, ipv4mask))
	if err != nil {
		return nil, fmt.Errorf("error parsing CIDR block: (%v)", err)
	}

	log.Infof("setting TUN interface address to [%s]", ip)
	err = luid.SetIPAddresses([]net.IPNet{{IP: ip, Mask: ipnet.Mask}})
	if err != nil {
		return nil, fmt.Errorf("failed to set IP address to %v: (%v)", ip, err)
	}

	dnsServers := []net.IP{ ip }

	log.Infof("adding DNS servers to TUN: %s", dnsServers)
	err = luid.AddDNS(dnsServers)
	if err != nil {
		return nil, fmt.Errorf("failed to add DNS addresses: (%v)", err)
	}

	log.Info("checking TUN dns servers")
	dns, err := luid.DNS()
	if err != nil {
		return nil, fmt.Errorf("failed to fetch DNS address: (%v)", err)
	}
	log.Infof("TUN dns servers set to: %s", dns)

	log.Infof("setting routes for cidr: %s. Next Hop: %s", ipnet.String(), ipnet.IP.String())
	err = luid.SetRoutes([]*winipcfg.RouteData{{ Destination: *ipnet, NextHop: ipnet.IP, Metric: 0}})
	if err != nil {
		return nil, fmt.Errorf("failed to SetRoutes: (%v)", err)
	}
	log.Info("routing applied")

	cziti.DnsInit(&rts, ipv4, ipv4mask)
	cziti.Start()
	err = cziti.HookupTun(tunDevice)
	if err != nil {
		log.Panicf("An unrecoverable error has occurred! %v", err)
	}

	return ip, nil
}

func (t *RuntimeState) LoadIdentity(id *Id) {
	log.Infof("loading identity %s[%s]", id.Name, id.FingerPrint)
	if id.CId != nil && id.CId.Loaded {
		log.Warnf("id %s[%s] already connected", id.Name, id.FingerPrint)
		return
	}

	id.CId = cziti.LoadZiti(id.Path())
	if id.CId == nil {
		log.Warnf("connecting to identity with fingerprint [%s] did not error but no context was returned", id.FingerPrint)
		return
	}

	id.ControllerVersion = id.CId.Version
	id.CId.Fingerprint = id.FingerPrint
	id.CId.Loaded = true
	id.Config.ZtAPI = id.CId.Controller()

	// hack for now - if the identity name is '<unknown>' don't set it... :(
	if id.CId.Name == "<unknown>" {
		log.Debugf("name is set to <unknown> which probably indicates the controller is down - not changing the name")
	} else if id.Name != id.CId.Name {
		log.Debugf("name changed from %s to %s", id.Name, id.CId.Name)
		id.Name = id.CId.Name
	}
	log.Infof("successfully loaded %s@%s", id.CId.Name, id.CId.Controller())
	_, found := t.ids[id.FingerPrint]
	if !found {
		t.ids[id.FingerPrint] = id //add this identity to the list of known ids
	}
}

func (t *RuntimeState) LoadConfig() {
	err := readConfig(t, config.File())
	if err != nil {
		err = readConfig(t, config.BackupFile())
		if err != nil {
			log.Panicf("config file is not valid nor is backup file!")
		}
	}

	//any specific code needed when starting the process. some values need to be cleared
	TunStarted = time.Now() //reset the time on startup

	if rts.state.TunIpv4Mask > constants.Ipv4MinMask {
		log.Warnf("provided mask: [%d] is smaller than the minimum permitted: [%d] and will be changed", rts.state.TunIpv4Mask, constants.Ipv4MinMask)
		rts.UpdateIpv4Mask(constants.Ipv4MinMask)
	}
}

func readConfig(t *RuntimeState, filename string) error {
	log.Infof("reading config file located at: %s", filename)
	info, err := os.Stat(filename)
	if os.IsNotExist(err) {
		log.Infof("the config file does not exist. this is normal if this is a new install or if the config file was removed manually")
		return nil
	}

	if info.Size() == 0 {
		return fmt.Errorf("the config file at contains no bytes and is considered invalid: %s", filename)
	}

	file, err := os.OpenFile(filename, os.O_RDONLY, 0644)
	if err != nil {
		return fmt.Errorf("unexpected error opening config file: %v", err)
	}

	r := bufio.NewReader(file)
	dec := json.NewDecoder(r)

	err = dec.Decode(&t.state)
	defer file.Close()

	if err != nil {
		return fmt.Errorf("unexpected error reading config file: %v", err)
	}
	return nil
}

func (t *RuntimeState) UpdateIpv4Mask(ipv4mask int){
	rts.state.TunIpv4Mask = ipv4mask
	rts.SaveState()
}
func (t *RuntimeState) UpdateIpv4(ipv4 string){
	rts.state.TunIpv4 = ipv4
	rts.SaveState()
}

// uses the registry to determine if IPv6 is enabled or disabled on this machine. If it is disabled an IPv6 DNS entry
// will end up causing a fatal error on startup of the service. For this registry key and values see the MS documentation
// at https://docs.microsoft.com/en-us/troubleshoot/windows-server/networking/configure-ipv6-in-windows
func iPv6Disabled() bool {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, `SYSTEM\CurrentControlSet\Services\Tcpip6\Parameters`, registry.QUERY_VALUE)
	if err != nil {
		log.Warnf("could not read registry to detect IPv6 - assuming IPv6 enabled. If IPv6 is not enabled the service may fail to start")
		return false
	}
	defer k.Close()

	val, _, err := k.GetIntegerValue("DisabledComponents")
	if err != nil {
		log.Debugf("registry key HKLM\\SYSTEM\\CurrentControlSet\\Services\\Tcpip6\\Parameters\\DisabledComponents not present. IPv6 is enabled")
		return false
	}
	actual := val & 255
	log.Debugf("read value from registry: %d. using actual: %d", val, actual)
	if actual == 255 {
		return true
	} else {
		log.Infof("IPv6 has DisabledComponents set to %d. If the service fails to start please report this message", val)
		return false
	}
}

func (t *RuntimeState) AddRoute(destination net.IPNet, nextHop net.IP, metric uint32) error {
	nativeTunDevice := (*t.tun).(*tun.NativeTun)
	luid := winipcfg.LUID(nativeTunDevice.LUID())
	return luid.AddRoute(destination, nextHop, metric)
}

func (t *RuntimeState) RemoveRoute(destination net.IPNet, nextHop net.IP) error {
	nativeTunDevice := (*t.tun).(*tun.NativeTun)
	luid := winipcfg.LUID(nativeTunDevice.LUID())
	return luid.DeleteRoute(destination, nextHop)
}


func (t *RuntimeState) Close() {
	if t.tun != nil {
		tu := *t.tun
		err := tu.Close()
		if err != nil {
			log.Fatalf("problem closing tunnel!")
		}
	} else {
		log.Warn("unexpected situation. the TUN was null? ")
	}
}


func (t *RuntimeState) InterceptDNS() {
	log.Panicf("implement me")
}

func (t *RuntimeState) ReleaseDNS() {
	log.Panicf("implement me")
}

func (t *RuntimeState) InterceptIP() {
	log.Panicf("implement me")
}

func (t *RuntimeState) ReleaseIP() {
	log.Panicf("implement me")
}
