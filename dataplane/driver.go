// Copyright (c) 2020 Tigera, Inc. All rights reserved.
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

// +build !windows

package dataplane

import (
	"math/bits"
	"net"
	"os/exec"

	"github.com/projectcalico/felix/wireguard"

	"github.com/projectcalico/felix/bpf/conntrack"

	"k8s.io/client-go/kubernetes"

	log "github.com/sirupsen/logrus"

	"runtime/debug"

	"github.com/projectcalico/felix/config"
	extdataplane "github.com/projectcalico/felix/dataplane/external"
	intdataplane "github.com/projectcalico/felix/dataplane/linux"
	"github.com/projectcalico/felix/idalloc"
	"github.com/projectcalico/felix/ifacemonitor"
	"github.com/projectcalico/felix/ipsets"
	"github.com/projectcalico/felix/logutils"
	"github.com/projectcalico/felix/markbits"
	"github.com/projectcalico/felix/rules"
	"github.com/projectcalico/libcalico-go/lib/health"
)

func StartDataplaneDriver(configParams *config.Config,
	healthAggregator *health.HealthAggregator,
	configChangedRestartCallback func(),
	k8sClientSet *kubernetes.Clientset) (DataplaneDriver, *exec.Cmd) {
	if configParams.UseInternalDataplaneDriver {
		log.Info("Using internal (linux) dataplane driver.")
		// If kube ipvs interface is present, enable ipvs support.
		kubeIPVSSupportEnabled := ifacemonitor.IsInterfacePresent(intdataplane.KubeIPVSInterface)
		if kubeIPVSSupportEnabled {
			log.Info("Kube-proxy in ipvs mode, enabling felix kube-proxy ipvs support.")
		}
		if configChangedRestartCallback == nil {
			log.Panic("Starting dataplane with nil callback func.")
		}

		markBitsManager := markbits.NewMarkBitsManager(configParams.IptablesMarkMask, "felix-iptables")
		// Dedicated mark bits for accept and pass actions.  These are long lived bits
		// that we use for communicating between chains.
		markAccept, _ := markBitsManager.NextSingleBitMark()
		markPass, _ := markBitsManager.NextSingleBitMark()

		var markWireguard uint32
		if configParams.WireguardEnabled {
			log.Info("Wireguard enabled, allocating a mark bit")
			markWireguard, _ = markBitsManager.NextSingleBitMark()
			if markWireguard == 0 {
				log.WithFields(log.Fields{
					"Name":     "felix-iptables",
					"MarkMask": configParams.IptablesMarkMask,
				}).Panic("Failed to allocate a mark bit for wireguard, not enough mark bits available.")
			}
		}

		// Short-lived mark bits for local calculations within a chain.
		markScratch0, _ := markBitsManager.NextSingleBitMark()
		markScratch1, _ := markBitsManager.NextSingleBitMark()
		if markAccept == 0 || markPass == 0 || markScratch0 == 0 || markScratch1 == 0 {
			log.WithFields(log.Fields{
				"Name":     "felix-iptables",
				"MarkMask": configParams.IptablesMarkMask,
			}).Panic("Not enough mark bits available.")
		}

		// Mark bits for end point mark. Currently felix takes the rest bits from mask available for use.
		markEndpointMark, allocated := markBitsManager.NextBlockBitsMark(markBitsManager.AvailableMarkBitCount())
		if kubeIPVSSupportEnabled && allocated == 0 {
			log.WithFields(log.Fields{
				"Name":     "felix-iptables",
				"MarkMask": configParams.IptablesMarkMask,
			}).Panic("Not enough mark bits available for endpoint mark.")
		}
		// Take lowest bit position (position 1) from endpoint mark mask reserved for non-calico endpoint.
		markEndpointNonCaliEndpoint := uint32(1) << uint(bits.TrailingZeros32(markEndpointMark))
		log.WithFields(log.Fields{
			"acceptMark":          markAccept,
			"passMark":            markPass,
			"scratch0Mark":        markScratch0,
			"scratch1Mark":        markScratch1,
			"endpointMark":        markEndpointMark,
			"endpointMarkNonCali": markEndpointNonCaliEndpoint,
		}).Info("Calculated iptables mark bits")

		// Create a routing table manager. There are certain components that should take specific indices in the range
		// to simplify table tidy-up.
		routeTableIndexAllocator := idalloc.NewIndexAllocator(configParams.RouteTableRange)

		// Always allocate the wireguard table index (even when not enabled). This ensures we can tidy up entries
		// if wireguard is disabled after being previously enabled.
		var wireguardEnabled bool
		var wireguardTableIndex int
		if idx, err := routeTableIndexAllocator.GrabIndex(); err == nil {
			log.Debugf("Assigned wireguard table index: %d", idx)
			wireguardEnabled = configParams.WireguardEnabled
			wireguardTableIndex = idx
		} else {
			log.WithError(err).Warning("Unable to assign table index for wireguard")
		}

		dpConfig := intdataplane.Config{
			Hostname: configParams.FelixHostname,
			IfaceMonitorConfig: ifacemonitor.Config{
				InterfaceExcludes: configParams.InterfaceExclude,
			},
			RulesConfig: rules.Config{
				WorkloadIfacePrefixes: configParams.InterfacePrefixes(),

				IPSetConfigV4: ipsets.NewIPVersionConfig(
					ipsets.IPFamilyV4,
					rules.IPSetNamePrefix,
					rules.AllHistoricIPSetNamePrefixes,
					rules.LegacyV4IPSetNames,
				),
				IPSetConfigV6: ipsets.NewIPVersionConfig(
					ipsets.IPFamilyV6,
					rules.IPSetNamePrefix,
					rules.AllHistoricIPSetNamePrefixes,
					nil,
				),

				KubeNodePortRanges:     configParams.KubeNodePortRanges,
				KubeIPVSSupportEnabled: kubeIPVSSupportEnabled,

				OpenStackSpecialCasesEnabled: configParams.OpenstackActive(),
				OpenStackMetadataIP:          net.ParseIP(configParams.MetadataAddr),
				OpenStackMetadataPort:        uint16(configParams.MetadataPort),

				IptablesMarkAccept:          markAccept,
				IptablesMarkPass:            markPass,
				IptablesMarkScratch0:        markScratch0,
				IptablesMarkScratch1:        markScratch1,
				IptablesMarkEndpoint:        markEndpointMark,
				IptablesMarkNonCaliEndpoint: markEndpointNonCaliEndpoint,

				VXLANEnabled: configParams.VXLANEnabled,
				VXLANPort:    configParams.VXLANPort,
				VXLANVNI:     configParams.VXLANVNI,

				IPIPEnabled:        configParams.IpInIpEnabled,
				IPIPTunnelAddress:  configParams.IpInIpTunnelAddr,
				IPIPTunnelInterfaceName: configParams.IpInIpTunnelInterfaceName,
				VXLANTunnelAddress: configParams.IPv4VXLANTunnelAddr,

				IptablesLogPrefix:         configParams.LogPrefix,
				EndpointToHostAction:      configParams.DefaultEndpointToHostAction,
				IptablesFilterAllowAction: configParams.IptablesFilterAllowAction,
				IptablesMangleAllowAction: configParams.IptablesMangleAllowAction,

				FailsafeInboundHostPorts:  configParams.FailsafeInboundHostPorts,
				FailsafeOutboundHostPorts: configParams.FailsafeOutboundHostPorts,

				DisableConntrackInvalid: configParams.DisableConntrackInvalidCheck,

				NATPortRange:                       configParams.NATPortRange,
				IptablesNATOutgoingInterfaceFilter: configParams.IptablesNATOutgoingInterfaceFilter,
				NATOutgoingAddress:                 configParams.NATOutgoingAddress,
				BPFEnabled:                         configParams.BPFEnabled,
			},
			Wireguard: wireguard.Config{
				Enabled:             wireguardEnabled,
				ListeningPort:       configParams.WireguardListeningPort,
				FirewallMark:        int(markWireguard),
				RoutingRulePriority: configParams.WireguardRoutingRulePriority,
				RoutingTableIndex:   wireguardTableIndex,
				InterfaceName:       configParams.WireguardInterfaceName,
				MTU:                 configParams.WireguardMTU,
			},
			IPIPMTU:                        configParams.IpInIpMtu,
			VXLANMTU:                       configParams.VXLANMTU,
			IptablesBackend:                configParams.IptablesBackend,
			IptablesRefreshInterval:        configParams.IptablesRefreshInterval,
			RouteRefreshInterval:           configParams.RouteRefreshInterval,
			DeviceRouteSourceAddress:       configParams.DeviceRouteSourceAddress,
			DeviceRouteProtocol:            configParams.DeviceRouteProtocol,
			RemoveExternalRoutes:           configParams.RemoveExternalRoutes,
			IPSetsRefreshInterval:          configParams.IpsetsRefreshInterval,
			IptablesPostWriteCheckInterval: configParams.IptablesPostWriteCheckIntervalSecs,
			IptablesInsertMode:             configParams.ChainInsertMode,
			IptablesLockFilePath:           configParams.IptablesLockFilePath,
			IptablesLockTimeout:            configParams.IptablesLockTimeoutSecs,
			IptablesLockProbeInterval:      configParams.IptablesLockProbeIntervalMillis,
			MaxIPSetSize:                   configParams.MaxIpsetSize,
			IPv6Enabled:                    configParams.Ipv6Support,
			StatusReportingInterval:        configParams.ReportingIntervalSecs,
			XDPRefreshInterval:             configParams.XDPRefreshInterval,

			NetlinkTimeout: configParams.NetlinkTimeoutSecs,

			ConfigChangedRestartCallback: configChangedRestartCallback,

			PostInSyncCallback: func() {
				// The initial resync uses a lot of scratch space so now is
				// a good time to force a GC and return any RAM that we can.
				debug.FreeOSMemory()

				if configParams.DebugMemoryProfilePath == "" {
					return
				}
				logutils.DumpHeapMemoryProfile(configParams.DebugMemoryProfilePath)
			},
			HealthAggregator:                   healthAggregator,
			DebugSimulateDataplaneHangAfter:    configParams.DebugSimulateDataplaneHangAfter,
			ExternalNodesCidrs:                 configParams.ExternalNodesCIDRList,
			SidecarAccelerationEnabled:         configParams.SidecarAccelerationEnabled,
			BPFEnabled:                         configParams.BPFEnabled,
			BPFDisableUnprivileged:             configParams.BPFDisableUnprivileged,
			BPFConnTimeLBEnabled:               configParams.BPFConnectTimeLoadBalancingEnabled,
			BPFKubeProxyIptablesCleanupEnabled: configParams.BPFKubeProxyIptablesCleanupEnabled,
			BPFLogLevel:                        configParams.BPFLogLevel,
			BPFDataIfacePattern:                configParams.BPFDataIfacePattern,
			BPFCgroupV2:                        configParams.DebugBPFCgroupV2,
			BPFMapRepin:                        configParams.DebugBPFMapRepinEnabled,
			KubeProxyMinSyncPeriod:             configParams.BPFKubeProxyMinSyncPeriod,
			XDPEnabled:                         configParams.XDPEnabled,
			XDPAllowGeneric:                    configParams.GenericXDPEnabled,
			BPFConntrackTimeouts:               conntrack.DefaultTimeouts(), // FIXME make timeouts configurable
			RouteTableManager:                  routeTableIndexAllocator,

			KubeClientSet: k8sClientSet,
		}

		if configParams.BPFExternalServiceMode == "dsr" {
			dpConfig.BPFNodePortDSREnabled = true
		}

		intDP := intdataplane.NewIntDataplaneDriver(dpConfig)
		intDP.Start()

		return intDP, nil
	} else {
		log.WithField("driver", configParams.DataplaneDriver).Info(
			"Using external dataplane driver.")

		return extdataplane.StartExtDataplaneDriver(configParams.DataplaneDriver)
	}
}
