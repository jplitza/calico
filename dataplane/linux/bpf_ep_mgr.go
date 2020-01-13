// Copyright (c) 2019 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package intdataplane

import (
	"bufio"
	"bytes"
	"fmt"
	"io/ioutil"
	"math/rand"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/projectcalico/felix/idalloc"

	"github.com/projectcalico/felix/ifacemonitor"

	"github.com/projectcalico/felix/bpf"

	log "github.com/sirupsen/logrus"

	"github.com/projectcalico/felix/proto"
	"github.com/projectcalico/libcalico-go/lib/set"
)

type epIface struct {
	ifacemonitor.State
	addrs []net.IP
}

type bpfEndpointManager struct {
	// Caches.  Updated immediately for now.
	wlEps    map[proto.WorkloadEndpointID]*proto.WorkloadEndpoint
	policies map[proto.PolicyID]*proto.Policy
	profiles map[proto.ProfileID]*proto.Profile
	ifaces   map[string]epIface

	// Indexes
	policiesToWorkloads map[proto.PolicyID]set.Set  /*proto.WorkloadEndpointID*/
	profilesToWorkloads map[proto.ProfileID]set.Set /*proto.WorkloadEndpointID*/

	dirtyWorkloads set.Set
	dirtyIfaces    set.Set

	bpfLogLevel      string
	fibLookupEnabled bool
	dataIfaceRegex   *regexp.Regexp
	ipSetIDAlloc     *idalloc.IDAllocator
	epToHostDrop     bool
	natTunnelMTU     int
}

func newBPFEndpointManager(
	bpfLogLevel string,
	fibLookupEnabled bool,
	epToHostDrop bool,
	dataIfaceRegex *regexp.Regexp,
	ipSetIDAlloc *idalloc.IDAllocator,
	natTunnelMTU int,
) *bpfEndpointManager {
	return &bpfEndpointManager{
		wlEps:               map[proto.WorkloadEndpointID]*proto.WorkloadEndpoint{},
		policies:            map[proto.PolicyID]*proto.Policy{},
		profiles:            map[proto.ProfileID]*proto.Profile{},
		ifaces:              map[string]epIface{},
		policiesToWorkloads: map[proto.PolicyID]set.Set{},
		profilesToWorkloads: map[proto.ProfileID]set.Set{},
		dirtyWorkloads:      set.New(),
		dirtyIfaces:         set.New(),
		bpfLogLevel:         bpfLogLevel,
		fibLookupEnabled:    fibLookupEnabled,
		dataIfaceRegex:      dataIfaceRegex,
		ipSetIDAlloc:        ipSetIDAlloc,
		epToHostDrop:        epToHostDrop,
		natTunnelMTU:        natTunnelMTU,
	}
}

func (m *bpfEndpointManager) OnUpdate(msg interface{}) {
	switch msg := msg.(type) {
	// Updates from the dataplane:

	// Interface updates.
	case *ifaceUpdate:
		m.onInterfaceUpdate(msg)
	case *ifaceAddrsUpdate:
		m.onInterfaceAddrsUpdate(msg)

	// Updates from the datamodel:

	// Workloads.
	case *proto.WorkloadEndpointUpdate:
		m.onWorkloadEndpointUpdate(msg)
	case *proto.WorkloadEndpointRemove:
		m.onWorkloadEnpdointRemove(msg)
	// Policies.
	case *proto.ActivePolicyUpdate:
		m.onPolicyUpdate(msg)
	case *proto.ActivePolicyRemove:
		m.onPolicyRemove(msg)
	// Profiles.
	case *proto.ActiveProfileUpdate:
		m.onProfileUpdate(msg)
	case *proto.ActiveProfileRemove:
		m.onProfileRemove(msg)
	}
}

func (m *bpfEndpointManager) onInterfaceUpdate(update *ifaceUpdate) {
	if update.State == ifacemonitor.StateUnknown {
		delete(m.ifaces, update.Name)
	} else {
		iface := m.ifaces[update.Name]
		iface.State = update.State
		m.ifaces[update.Name] = iface
	}
	m.dirtyIfaces.Add(update.Name)
}

func (m *bpfEndpointManager) onInterfaceAddrsUpdate(update *ifaceAddrsUpdate) {
	var addrs []net.IP

	if update == nil || update.Addrs == nil {
		return
	}

	update.Addrs.Iter(func(s interface{}) error {
		str, ok := s.(string)
		if !ok {
			log.WithField("addr", s).Errorf("wrong type %T", s)
			return nil
		}
		ip := net.ParseIP(str)
		if ip == nil {
			return nil
		}
		ip = ip.To4()
		if ip == nil {
			return nil
		}
		addrs = append(addrs, ip)
		return nil
	})

	iface := m.ifaces[update.Name]
	iface.addrs = addrs
	m.ifaces[update.Name] = iface
	m.dirtyIfaces.Add(update.Name)
	log.WithField("iface", update.Name).WithField("addrs", addrs).WithField("State", iface.State).
		Debugf("onInterfaceAddrsUpdate")
}

// onWorkloadEndpointUpdate adds/updates the workload in the cache along with the index from active policy to
// workloads using that policy.
func (m *bpfEndpointManager) onWorkloadEndpointUpdate(msg *proto.WorkloadEndpointUpdate) {
	log.WithField("wep", msg.Endpoint).Debug("Workload endpoint update")
	wlID := *msg.Id
	oldWL := m.wlEps[wlID]
	wl := msg.Endpoint
	if oldWL != nil {
		for _, t := range oldWL.Tiers {
			for _, pol := range t.IngressPolicies {
				polSet := m.policiesToWorkloads[proto.PolicyID{
					Tier: t.Name,
					Name: pol,
				}]
				if polSet == nil {
					continue
				}
				polSet.Discard(wlID)
			}
			for _, pol := range t.EgressPolicies {
				polSet := m.policiesToWorkloads[proto.PolicyID{
					Tier: t.Name,
					Name: pol,
				}]
				if polSet == nil {
					continue
				}
				polSet.Discard(wlID)
			}
		}

		for _, profName := range oldWL.ProfileIds {
			profID := proto.ProfileID{Name: profName}
			profSet := m.profilesToWorkloads[profID]
			if profSet == nil {
				continue
			}
			profSet.Discard(wlID)
		}
	}
	m.wlEps[wlID] = msg.Endpoint
	for _, t := range wl.Tiers {
		for _, pol := range t.IngressPolicies {
			polID := proto.PolicyID{
				Tier: t.Name,
				Name: pol,
			}
			if m.policiesToWorkloads[polID] == nil {
				m.policiesToWorkloads[polID] = set.New()
			}
			m.policiesToWorkloads[polID].Add(wlID)
		}
		for _, pol := range t.EgressPolicies {
			polID := proto.PolicyID{
				Tier: t.Name,
				Name: pol,
			}
			if m.policiesToWorkloads[polID] == nil {
				m.policiesToWorkloads[polID] = set.New()
			}
			m.policiesToWorkloads[polID].Add(wlID)
		}
		for _, profName := range wl.ProfileIds {
			profID := proto.ProfileID{Name: profName}
			profSet := m.profilesToWorkloads[profID]
			if profSet == nil {
				profSet = set.New()
				m.profilesToWorkloads[profID] = profSet
			}
			profSet.Add(wlID)
		}
	}
	m.dirtyWorkloads.Add(wlID)
}

// onWorkloadEndpointRemove removes the workload from the cache and the index, which maps from policy to workload.
func (m *bpfEndpointManager) onWorkloadEnpdointRemove(msg *proto.WorkloadEndpointRemove) {
	wlID := *msg.Id
	log.WithField("id", wlID).Debug("Workload endpoint removed")
	wl := m.wlEps[wlID]
	for _, t := range wl.Tiers {
		for _, pol := range t.IngressPolicies {
			polSet := m.policiesToWorkloads[proto.PolicyID{
				Tier: t.Name,
				Name: pol,
			}]
			if polSet == nil {
				continue
			}
			polSet.Discard(wlID)
		}
		for _, pol := range t.EgressPolicies {
			polSet := m.policiesToWorkloads[proto.PolicyID{
				Tier: t.Name,
				Name: pol,
			}]
			if polSet == nil {
				continue
			}
			polSet.Discard(wlID)
		}
	}
	delete(m.wlEps, wlID)
	m.dirtyWorkloads.Add(wlID)
}

// onPolicyUpdate stores the policy in the cache and marks any endpoints using it dirty.
func (m *bpfEndpointManager) onPolicyUpdate(msg *proto.ActivePolicyUpdate) {
	polID := *msg.Id
	log.WithField("id", polID).Debug("Policy update")
	m.policies[polID] = msg.Policy
	m.markPolicyUsersDirty(polID)
}

// onPolicyRemove removes the policy from the cache and marks any endpoints using it dirty.
// The latter should be a no-op due to the ordering guarantees of the calc graph.
func (m *bpfEndpointManager) onPolicyRemove(msg *proto.ActivePolicyRemove) {
	polID := *msg.Id
	log.WithField("id", polID).Debug("Policy removed")
	m.markPolicyUsersDirty(polID)
	delete(m.policies, polID)
	delete(m.policiesToWorkloads, polID)
}

// onProfileUpdate stores the profile in the cache and marks any endpoints that use it as dirty.
func (m *bpfEndpointManager) onProfileUpdate(msg *proto.ActiveProfileUpdate) {
	profID := *msg.Id
	log.WithField("id", profID).Debug("Profile update")
	m.profiles[profID] = msg.Profile
	m.markProfileUsersDirty(profID)
}

// onProfileRemove removes the profile from the cache and marks any endpoints that were using it as dirty.
// The latter should be a no-op due to the ordering guarantees of the calc graph.
func (m *bpfEndpointManager) onProfileRemove(msg *proto.ActiveProfileRemove) {
	profID := *msg.Id
	log.WithField("id", profID).Debug("Profile removed")
	m.markProfileUsersDirty(profID)
	delete(m.profiles, profID)
	delete(m.profilesToWorkloads, profID)
}

func (m *bpfEndpointManager) markPolicyUsersDirty(id proto.PolicyID) {
	wls := m.policiesToWorkloads[id]
	if wls == nil {
		// Hear about the policy before the endpoint.
		return
	}
	wls.Iter(func(item interface{}) error {
		m.dirtyWorkloads.Add(item)
		return nil
	})
}

func (m *bpfEndpointManager) markProfileUsersDirty(id proto.ProfileID) {
	wls := m.profilesToWorkloads[id]
	if wls == nil {
		// Hear about the policy before the endpoint.
		return
	}
	wls.Iter(func(item interface{}) error {
		m.dirtyWorkloads.Add(item)
		return nil
	})
}

func (m *bpfEndpointManager) CompleteDeferredWork() error {
	m.applyProgramsToDirtyDataInterfaces()
	m.applyProgramsToDirtyWorkloadEndpoints()

	// TODO: handle cali interfaces with no WEP
	return nil
}

func (m *bpfEndpointManager) setAcceptLocal(iface string, val bool) error {
	numval := "0"
	if val {
		numval = "1"
	}

	path := fmt.Sprintf("/proc/sys/net/ipv4/conf/%s/accept_local", iface)
	err := writeProcSys(path, numval)
	if err != nil {
		log.WithField("err", err).Errorf("Failed to  set %s to %s", path, numval)
		return err
	}

	log.Infof("%s set to %s", path, numval)
	return nil
}

func (m *bpfEndpointManager) applyProgramsToDirtyDataInterfaces() {
	var mutex sync.Mutex
	errs := map[string]error{}
	var wg sync.WaitGroup
	m.dirtyIfaces.Iter(func(item interface{}) error {
		iface := item.(string)
		if !m.dataIfaceRegex.MatchString(iface) {
			log.WithField("iface", iface).Debug(
				"Ignoring interface that doesn't match the host data interface regex")
			return set.RemoveItem
		}
		if m.ifaces[iface].State != ifacemonitor.StateUp {
			log.WithField("iface", iface).Debug("Ignoring interface that is down")
			return set.RemoveItem
		}

		wg.Add(1)
		go func() {
			defer wg.Done()
			m.ensureQdisc(iface)
			err := m.compileAndAttachDataIfaceProgram(iface, PolDirnIngress)
			if err == nil {
				err = m.compileAndAttachDataIfaceProgram(iface, PolDirnEgress)
			}
			if err == nil {
				// This is required to allow NodePort forwarding with
				// encapsulation with the host's IP as the source address
				err = m.setAcceptLocal(iface, true)
			}
			mutex.Lock()
			errs[iface] = err
			mutex.Unlock()
		}()
		return nil
	})
	wg.Wait()
	m.dirtyIfaces.Iter(func(item interface{}) error {
		iface := item.(string)
		err := errs[iface]
		if err == nil {
			log.WithField("id", iface).Info("Applied program to host interface")
			return set.RemoveItem
		}
		log.WithError(err).Warn("Failed to apply policy to interface")
		return nil
	})
}

func (m *bpfEndpointManager) applyProgramsToDirtyWorkloadEndpoints() {
	var mutex sync.Mutex
	errs := map[proto.WorkloadEndpointID]error{}
	var wg sync.WaitGroup
	m.dirtyWorkloads.Iter(func(item interface{}) error {
		wg.Add(1)
		go func() {
			defer wg.Done()
			wlID := item.(proto.WorkloadEndpointID)
			err := m.applyPolicy(wlID)
			mutex.Lock()
			errs[wlID] = err
			mutex.Unlock()
		}()
		return nil
	})
	wg.Wait()
	m.dirtyWorkloads.Iter(func(item interface{}) error {
		wlID := item.(proto.WorkloadEndpointID)
		err := errs[wlID]
		if err == nil {
			log.WithField("id", wlID).Info("Applied policy to workload")
			return set.RemoveItem
		}
		log.WithError(err).Warn("Failed to apply policy to endpoint")
		return nil
	})
}

// applyPolicy actually applies the policy to the given workload.
func (m *bpfEndpointManager) applyPolicy(wlID proto.WorkloadEndpointID) error {
	startTime := time.Now()
	wep := m.wlEps[wlID]
	if wep == nil {
		// TODO clean up old workloads
		return nil
	}
	ifaceName := wep.Name

	m.ensureQdisc(ifaceName)

	var ingressErr, egressErr error
	var wg sync.WaitGroup

	wg.Add(2)
	go func() {
		defer wg.Done()
		ingressErr = m.compileAndAttachWorkloadProgram(wep, PolDirnIngress)
	}()
	go func() {
		defer wg.Done()
		egressErr = m.compileAndAttachWorkloadProgram(wep, PolDirnEgress)
	}()
	wg.Wait()

	if ingressErr != nil {
		return ingressErr
	}
	if egressErr != nil {
		return egressErr
	}

	applyTime := time.Since(startTime)
	log.WithField("timeTaken", applyTime).Info("Finished applying BPF programs for workload")
	return nil
}

// EnsureQdisc makes sure that qdisc is attached to the given interface
func EnsureQdisc(ifaceName string) {
	// FIXME Avoid flapping the tc program and qdisc
	cmd := exec.Command("tc", "qdisc", "del", "dev", ifaceName, "clsact")
	_ = cmd.Run()
	cmd = exec.Command("tc", "qdisc", "add", "dev", ifaceName, "clsact")
	_ = cmd.Run()
}

func (m *bpfEndpointManager) ensureQdisc(ifaceName string) {
	EnsureQdisc(ifaceName)
}

func (m *bpfEndpointManager) compileAndAttachWorkloadProgram(endpoint *proto.WorkloadEndpoint, polDirection PolDirection) error {
	rules := m.extractRules(endpoint.Tiers, endpoint.ProfileIds, polDirection)
	ap := calculateTCAttachPoint(EpTypeWorkload, polDirection, endpoint.Name)
	return m.compileAndAttachProgram(rules, ap)
}

func (m *bpfEndpointManager) compileAndAttachDataIfaceProgram(ifaceName string, polDirection PolDirection) error {
	rules := [][][]*proto.Rule{{{{Action: "Allow"}}}}
	epType := EpTypeHost
	if ifaceName == "tunl0" {
		epType = EpTypeTunnel
	}
	ap := calculateTCAttachPoint(epType, polDirection, ifaceName)
	return m.compileAndAttachProgram(rules, ap)
}

// PolDirection is the Calico datamodel direction of policy.  On a host endpoint, ingress is towards the host.
// On a workload endpoint, ingress is towards the workload.
type PolDirection string

const (
	PolDirnIngress PolDirection = "ingress"
	PolDirnEgress  PolDirection = "egress"
)

// TCHook is the hook to which a BPF program should be attached.  This is relative to the host namespace
// so workload PolDirnIngress policy is attached to the TCHookEgress.
type TCHook string

const (
	TCHookIngress TCHook = "ingress"
	TCHookEgress  TCHook = "egress"
)

type TCAttachPoint struct {
	Section      string
	Hook         TCHook
	Iface        string
	CompileFlags int
}

const (
	CompileFlagHostEp  = 1
	CompileFlagIngress = 2
	CompileFlagTunnel  = 4
	CompileFlagCgroup  = 8
)

type ToOrFromEp string

const (
	FromEp ToOrFromEp = "from"
	ToEp   ToOrFromEp = "to"
)

type EndpointType string

const (
	EpTypeWorkload EndpointType = "workload"
	EpTypeHost     EndpointType = "host"
	EpTypeTunnel   EndpointType = "tunnel"
)

func calculateTCAttachPoint(endpointType EndpointType, policyDirection PolDirection, ifaceName string) TCAttachPoint {
	var ap TCAttachPoint

	if endpointType == EpTypeWorkload {
		// Policy direction is relative to the workload so, from the host namespace it's flipped.
		if policyDirection == PolDirnIngress {
			ap.Hook = TCHookEgress
		} else {
			ap.Hook = TCHookIngress
		}
	} else {
		// Host endpoints have the natural relationship between policy direction and hook.
		if policyDirection == PolDirnIngress {
			ap.Hook = TCHookIngress
		} else {
			ap.Hook = TCHookEgress
		}
	}

	var toOrFrom ToOrFromEp
	if ap.Hook == TCHookIngress {
		toOrFrom = FromEp
	} else {
		toOrFrom = ToEp
	}

	ap.Section = BPFSectionName(endpointType, toOrFrom)
	ap.CompileFlags = BPFSectionToFlags(ap.Section)
	ap.Iface = ifaceName

	return ap
}

func BPFSectionName(endpointType EndpointType, fromOrTo ToOrFromEp) string {
	return fmt.Sprintf("calico_%s_%s_ep", fromOrTo, endpointType)
}

var bpfSectionToFlags = map[string]int{}

func init() {
	bpfSectionToFlags[BPFSectionName(EpTypeWorkload, FromEp)] = 0
	bpfSectionToFlags[BPFSectionName(EpTypeWorkload, ToEp)] = CompileFlagIngress
	bpfSectionToFlags[BPFSectionName(EpTypeHost, FromEp)] = CompileFlagHostEp | CompileFlagIngress
	bpfSectionToFlags[BPFSectionName(EpTypeHost, ToEp)] = CompileFlagHostEp
	bpfSectionToFlags[BPFSectionName(EpTypeTunnel, FromEp)] = CompileFlagHostEp | CompileFlagIngress | CompileFlagTunnel
	bpfSectionToFlags[BPFSectionName(EpTypeTunnel, ToEp)] = CompileFlagHostEp | CompileFlagTunnel
}

// BPFSectionToFlags is used by the UTs to magic the correct flags given a section name.
func BPFSectionToFlags(section string) int {
	flags, ok := bpfSectionToFlags[section]
	if !ok {
		log.WithField("section", section).Panic("Bug: unknown BPF section name")
	}
	return flags
}

func (m *bpfEndpointManager) extractRules(tiers2 []*proto.TierInfo, profileNames []string, direction PolDirection) [][][]*proto.Rule {
	var allRules [][][]*proto.Rule
	for _, tier := range tiers2 {
		var pols [][]*proto.Rule

		directionalPols := tier.IngressPolicies
		if direction == PolDirnEgress {
			directionalPols = tier.EgressPolicies
		}

		if len(directionalPols) == 0 {
			continue
		}

		for _, polName := range directionalPols {
			pol := m.policies[proto.PolicyID{Tier: tier.Name, Name: polName}]
			if direction == PolDirnIngress {
				pols = append(pols, pol.InboundRules)
			} else {
				pols = append(pols, pol.OutboundRules)
			}
		}
		allRules = append(allRules, pols)
	}
	var profs [][]*proto.Rule
	for _, profName := range profileNames {
		prof := m.profiles[proto.ProfileID{Name: profName}]
		if direction == PolDirnIngress {
			profs = append(profs, prof.InboundRules)
		} else {
			profs = append(profs, prof.OutboundRules)
		}
	}
	allRules = append(allRules, profs)
	return allRules
}

func (m *bpfEndpointManager) compileAndAttachProgram(allRules [][][]*proto.Rule, attachPoint TCAttachPoint) error {
	logCxt := log.WithField("attachPoint", attachPoint)

	tempDir, err := ioutil.TempDir("", "calico-compile")
	if err != nil {
		log.WithError(err).Panic("Failed to make temporary directory")
	}
	defer func() {
		_ = os.RemoveAll(tempDir)
	}()
	logCxt.WithField("tempDir", tempDir).Debug("Compiling program in temporary dir")
	srcDir := "/code/bpf/xdp"
	srcFileName := srcDir + "/redir_tc.c"
	oFileName := tempDir + "/redir_tc.o"
	logLevel := strings.ToUpper(m.bpfLogLevel)
	if logLevel == "" {
		logLevel = "OFF"
	}

	logPfx := os.Getenv("BPF_LOG_PFX") + attachPoint.Iface

	opts := []CompileTCOption{
		CompileWithWorkingDir(srcDir),
		CompileWithSourceName(srcFileName),
		CompileWithOutputName(oFileName),
		CompileWithFIBEnabled(m.fibLookupEnabled),
		CompileWithLogLevel(logLevel),
		CompileWithLogPrefix(logPfx),
		CompileWithEndpointToHostDrop(m.epToHostDrop),
		CompileWithNATTunnelMTU(uint16(m.natTunnelMTU)),
		CompileWithEntrypointName(attachPoint.Section),
		CompileWithFlags(attachPoint.CompileFlags),
	}

	iface := m.ifaces[attachPoint.Iface]
	if len(iface.addrs) > 0 {
		// XXX we currently support only a single IP on any iface and we pick
		// whichever is the first if there are more
		opts = append(opts, CompileWithHostIP(iface.addrs[0]))
	}

	err = CompileTCProgramToFile(allRules, m.ipSetIDAlloc, opts...)
	if err != nil {
		return err
	}

	err = AttachTCProgram(oFileName, attachPoint)
	if err != nil {
		logCxt.WithError(err).Error("Failed to attach BPF program")

		var buf bytes.Buffer
		pg, err := bpf.NewProgramGenerator(srcFileName, m.ipSetIDAlloc)
		if err != nil {
			logCxt.WithError(err).Panic("Failed to write get code generator.")
		}
		err = pg.WriteProgram(&buf, allRules)
		if err != nil {
			logCxt.WithError(err).Panic("Failed to write C file to buffer.")
		}

		logCxt.WithField("program", buf.String()).Error("Dump of program")
		return err
	}
	return nil
}

var tcLock sync.Mutex

// AttachTCProgram attaches a BPF program from a file to the TC attach point
func AttachTCProgram(fname string, attachPoint TCAttachPoint) error {
	// When tc is pinning maps, it is vulnerable to lost updates. Serialise tc calls.
	tcLock.Lock()
	defer tcLock.Unlock()

	// Work around tc map name collision: when we load two identical BPF programs onto different interfaces, tc
	// pins object-local maps to a namespace based on the hash of the BPF program, which is the same for both
	// interfaces.  Since we want one map per interface instead, we search for such maps and rename them before we
	// release the tc lock.
	//
	// For our purposes, it should work to simply delete the map.  However, when we tried that, the contents of the
	// map get deleted even though it is in use by a BPF program.
	defer func() {
		// Find the maps we care about by walking the BPF filesystem.
		err := filepath.Walk("/sys/fs/bpf/tc", func(path string, info os.FileInfo, err error) error {
			if err != nil {
				log.WithError(err).Panic("Failed to walk BPF filesystem")
				return err
			}
			if info.Name() == "cali_jump" {
				log.WithField("path", path).Debug("Queueing deletion of map")
				out, err := exec.Command("bpftool", "map", "show", "pinned", path).Output()
				if err != nil {
					log.WithError(err).Panic("Failed to show map")
				}
				log.WithField("dump", string(out)).Info("Map show before deletion")
				id := string(bytes.Split(out, []byte(":"))[0])

				// TODO: make a path based on the name of the interface and the hook so we can look it up later.
				out, err = exec.Command("bpftool", "map", "pin", "id", id, path+fmt.Sprint(rand.Uint32())).Output()
				if err != nil {
					log.WithError(err).Panic("Failed to repin map")
				}
				log.WithField("dump", string(out)).Info("Map pin before deletion")

				err = os.Remove(path)
				if err != nil {
					log.WithError(err).Panic("Failed to remove old map pin")
				}
			}
			return nil
		})
		if err != nil {
			log.WithError(err).Panic("Failed to walk BPF filesystem")
		}
		log.Debug("Finished moving map pins that we don't need.")
	}()

	tc := exec.Command("tc",
		"filter", "add", "dev", attachPoint.Iface,
		string(attachPoint.Hook),
		"bpf", "da", "obj", fname,
		"sec", attachPoint.Section)

	out, err := tc.CombinedOutput()
	if err != nil {
		if bytes.Contains(out, []byte("Cannot find device")) {
			// Avoid a big, spammy log when the issue is that the interface isn't present.
			log.WithField("iface", attachPoint.Iface).Warn(
				"Failed to attach BPF program; interface not found.  Will retry if it show up.")
			return nil
		}
		log.WithError(err).WithFields(log.Fields{"out": string(out)}).
			WithField("command", tc).Error("Failed to attach BPF program")
	}

	return err
}

// CompileTCOption specifies additional compile options for TC programs
type CompileTCOption func(*compileTCOpts)

type compileTCOpts struct {
	extraArgs []string
	dir       string
	srcFile   string
	outFile   string
	bpftool   bool
}

func (o *compileTCOpts) appendExtraArg(a string) {
	o.extraArgs = append(o.extraArgs, a)
}

// CompileWithEndpointToHostDrop sets whether workload-to-host traffic is dropped.
func CompileWithEndpointToHostDrop(drop bool) CompileTCOption {
	return func(opts *compileTCOpts) {
		opts.appendExtraArg(fmt.Sprintf("-DCALI_DROP_WORKLOAD_TO_HOST=%v", drop))
	}
}

// CompileWithDefine makes a -Dname defined
func CompileWithDefine(name string) CompileTCOption {
	return func(opts *compileTCOpts) {
		opts.appendExtraArg(fmt.Sprintf("-D%s", name))
	}
}

// CompileWithDefineValue makes a -Dname=value defined
func CompileWithDefineValue(name string, value string) CompileTCOption {
	return func(opts *compileTCOpts) {
		opts.appendExtraArg(fmt.Sprintf("-D%s=%s", name, value))
	}
}

// CompileWithEntrypointName controls the name of the BPF section entrypoint.
func CompileWithEntrypointName(name string) CompileTCOption {
	return CompileWithDefineValue("CALI_ENTRYPOINT_NAME", name)
}

// CompileWithIncludePath adds an include path to search for includes
func CompileWithIncludePath(p string) CompileTCOption {
	return func(opts *compileTCOpts) {
		opts.appendExtraArg(fmt.Sprintf("-I%s", p))
	}
}

// CompileWithFIBEnabled sets whether FIB lookup is allowed
func CompileWithFIBEnabled(enabled bool) CompileTCOption {
	return func(opts *compileTCOpts) {
		opts.appendExtraArg(fmt.Sprintf("-DCALI_FIB_LOOKUP_ENABLED=%v", enabled))
	}
}

// CompileWithLogLevel sets the log level of the resulting program
func CompileWithLogLevel(level string) CompileTCOption {
	return func(opts *compileTCOpts) {
		opts.appendExtraArg(fmt.Sprintf("-DCALI_LOG_LEVEL=CALI_LOG_LEVEL_%s", level))
	}
}

// CompileWithLogPrefix sets a specific log prefix for the resulting program
func CompileWithLogPrefix(prefix string) CompileTCOption {
	return func(opts *compileTCOpts) {
		opts.appendExtraArg(fmt.Sprintf("-DCALI_LOG_PFX=%v", prefix))
	}
}

// CompileWithSourceName sets the source file name
func CompileWithSourceName(f string) CompileTCOption {
	return func(opts *compileTCOpts) {
		opts.srcFile = f
	}
}

// CompileWithOutputName sets the output name
func CompileWithOutputName(f string) CompileTCOption {
	return func(opts *compileTCOpts) {
		opts.outFile = f
	}
}

// CompileWithWorkingDir sets the working directory
func CompileWithWorkingDir(dir string) CompileTCOption {
	return func(opts *compileTCOpts) {
		opts.dir = dir
	}
}

// CompileWithBpftoolLoader makes the result loadable by bpftool (in contrast to
// iproute2 only)
func CompileWithBpftoolLoader() CompileTCOption {
	return func(opts *compileTCOpts) {
		opts.bpftool = true
	}
}

// CompileWithHostIP makes the host ip available for the bpf code
func CompileWithHostIP(ip net.IP) CompileTCOption {
	ipv4 := ip.To4()
	if ipv4 == nil {
		return CompileWithDefineValue("CALI_HOST_IP", "bad-host-ip")
	}
	return CompileWithDefineValue("CALI_HOST_IP",
		fmt.Sprintf("0x%02x%02x%02x%02x", ipv4[3], ipv4[2], ipv4[1], ipv4[0]))
}

// CompileWithVxlanPort sets the VXLAN port to use to override the IANA default
func CompileWithVxlanPort(port uint16) CompileTCOption {
	return CompileWithDefineValue("CALI_VXLAN_PORT", fmt.Sprintf("%d", port))
}

// CompileWithNATTunnelMTU sets the MTU for NAT tunnel
func CompileWithNATTunnelMTU(mtu uint16) CompileTCOption {
	return CompileWithDefineValue("CALI_NAT_TUNNEL_MTU", fmt.Sprintf("%d", mtu))
}

// CompileWithFlags sets the CALI_COMPILE_FLAGS value.
func CompileWithFlags(flags int) CompileTCOption {
	return CompileWithDefineValue("CALI_COMPILE_FLAGS", fmt.Sprint(flags))
}

// CompileTCProgramToFile takes policy rules and compiles them into a tc-bpf
// program and saves it into the provided file. Extra CFLAGS can be provided
func CompileTCProgramToFile(allRules [][][]*proto.Rule, ipSetIDAlloc *idalloc.IDAllocator, opts ...CompileTCOption) error {
	compileOpts := compileTCOpts{
		srcFile: "/code/bpf/xdp/redir_tc.c",
		outFile: "/tmp/redir_tc.o",
		dir:     "/code/bpf/xdp",
	}

	for _, o := range opts {
		o(&compileOpts)
	}

	args := []string{
		"-x",
		"c",
		"-D__KERNEL__",
		"-D__ASM_SYSREG_H",
	}

	if compileOpts.bpftool {
		args = append(args, "-D__BPFTOOL_LOADER__")
	}

	args = append(args, compileOpts.extraArgs...)

	args = append(args, []string{
		"-I" + compileOpts.dir,
		"-Wno-unused-value",
		"-Wno-pointer-sign",
		"-Wno-compare-distinct-pointer-types",
		"-Wunused",
		"-Wall",
		"-Werror",
		"-fno-stack-protector",
		"-O2",
		"-emit-llvm",
		"-c", "-", "-o", "-",
	}...)
	log.WithField("args", args).Debug("About to run clang")
	clang := exec.Command("clang", args...)
	clang.Dir = compileOpts.dir
	clangStdin, err := clang.StdinPipe()
	if err != nil {
		return err
	}
	clangStdout, err := clang.StdoutPipe()
	if err != nil {
		return err
	}
	clangStderr, err := clang.StderrPipe()
	if err != nil {
		return err
	}

	log.WithField("command", clang.String()).Infof("compiling bpf")

	err = clang.Start()
	if err != nil {
		log.WithError(err).Panic("Failed to write C file.")
		return err
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		scanner := bufio.NewScanner(clangStderr)
		for scanner.Scan() {
			log.Warnf("clang stderr: %s", scanner.Text())
		}
		if err != nil {
			log.WithError(err).Error("Error while reading clang stderr")
		}
	}()
	wg.Add(1)
	go func() {
		defer wg.Done()
		pg, err := bpf.NewProgramGenerator(compileOpts.srcFile, ipSetIDAlloc)
		if err != nil {
			log.WithError(err).Panic("Failed to create code generator")
		}
		err = pg.WriteProgram(clangStdin, allRules)
		if err != nil {
			log.WithError(err).Panic("Failed to write C file.")
		}
		err = clangStdin.Close()
		if err != nil {
			log.WithError(err).Panic("Failed to write C file to clang stdin (Close() failed).")
		}
	}()
	llc := exec.Command("llc", "-march=bpf", "-filetype=obj", "-o", compileOpts.outFile)
	llc.Stdin = clangStdout
	out, err := llc.CombinedOutput()
	if err != nil {
		log.WithError(err).WithField("out", string(out)).Error("Failed to compile C program (llc step)")
		return err
	}
	err = clang.Wait()
	if err != nil {
		log.WithError(err).Error("Clang failed.")
		return err
	}
	wg.Wait()

	return nil
}
