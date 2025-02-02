// Copyright 2022 The Amesh Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//    http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.
//
package main

import (
	"os"
	"os/user"
	"strings"

	"github.com/spf13/cobra"
	"istio.io/istio/tools/istio-iptables/pkg/builder"
	iptablesconf "istio.io/istio/tools/istio-iptables/pkg/config"
	"istio.io/istio/tools/istio-iptables/pkg/dependencies"
	"istio.io/istio/tools/istio-iptables/pkg/log"
)

const (
	InboundChain         = "APISIX_INBOUND"
	RedirectChain        = "APISIX_REDIRECT"
	InboundRedirectChain = "APISIX_INBOUND_REDIRECT"
	OutputChain          = "OUTPUT"
	PreRoutingChain      = "PREROUTING"
)

type iptablesConstructor struct {
	iptables *builder.IptablesBuilder
	cfg      *iptablesconf.Config
	dep      dependencies.Dependencies
}

func newCommand() *cobra.Command {
	var (
		cfg       iptablesconf.Config
		proxyUser string
	)
	cmd := &cobra.Command{
		Use:   "amesh-iptables [flags]",
		Short: "Setting up iptables rules for port forwarding",
		Long: `Setting up iptables rules for port forwarding.

Intercept inbound TCP traffic which destination port is 80 to 9080 (apisix port), run:
	amesh-iptables --apisix-port 9080 --inbound-ports 80

To intercept all inbound TCP traffic, just use "*" as the value of --inbound-ports option. In addition,
if outbound TCP traffic (say the destination port is 80) is desired to be intercepted, just run:
	amesh-iptables --apisix-port 9080 --inbound-ports 80 --outbound-ports 80

--dry-run option can be specified if you just want to see which rules will be generated (but no effects).
`,
		Run: func(cmd *cobra.Command, args []string) {
			var dep dependencies.Dependencies
			if cfg.DryRun {
				dep = &dependencies.StdoutStubDependencies{}
			} else {
				dep = &dependencies.RealDependencies{}
			}

			usr, err := user.Lookup(proxyUser)
			if err != nil {
				panic(err)
			}
			cfg.ProxyUID = usr.Uid
			cfg.ProxyGID = usr.Gid

			ic := &iptablesConstructor{
				iptables: builder.NewIptablesBuilder(&cfg),
				cfg:      &cfg,
				dep:      dep,
			}

			ic.run()
		},
	}

	cmd.PersistentFlags().StringVar(&cfg.InboundInterceptionMode, "inbound-interception-mode", "REDIRECT",
		"iptables mode to redirect inbound connections")
	cmd.PersistentFlags().StringVar(&cfg.InboundCapturePort, "apisix-inbound-capture-port", "9081", "the target port where all inbound TCP traffic should be redirected on")
	cmd.PersistentFlags().StringVar(&cfg.ProxyPort, "apisix-port", "9080", "the target port where all TCP traffic should be redirected on")
	cmd.PersistentFlags().StringVar(&cfg.InboundPortsInclude, "inbound-ports", "",
		"comma separated list of inbound ports for which traffic is to be redirected, the wildcard character \"*\" can be used to configure redirection for all ports, empty list will disable the redirection")
	cmd.PersistentFlags().StringVar(&cfg.OutboundPortsInclude, "outbound-ports", "", "comma separated list of outbound ports for which traffic is to be redirected")
	cmd.PersistentFlags().StringVar(&cfg.InboundPortsExclude, "inbound-exclude-ports", "", "comma separated list of inbound ports to be excluded from forwarding to APISIX, only in effective if value of --inbound-ports option is \"*\"")
	cmd.PersistentFlags().StringVar(&cfg.OutboundPortsExclude, "outbound-exclude-ports", "", "comma separated list of outbound ports to be excluded from forwarding to APISIX, only in effective if value of --outbound-ports option is \"*\"")

	cmd.PersistentFlags().BoolVar(&cfg.DryRun, "dry-run", false, "dry run mode")
	cmd.PersistentFlags().StringVar(&proxyUser, "apisix-user", "nobody", "user to run APISIX")

	return cmd
}

func (ic *iptablesConstructor) run() {
	ic.iptables.AppendRuleV4(
		log.UndefinedCommand, RedirectChain, "nat", "-p", "tcp", "-j", "REDIRECT", "--to-ports", ic.cfg.ProxyPort,
	)
	ic.iptables.AppendRuleV4(
		log.UndefinedCommand, InboundRedirectChain, "nat", "-p", "tcp",
		"-j", "REDIRECT", "--to-ports", ic.cfg.InboundCapturePort,
	)

	// Should first insert these skipping rules.
	ic.insertSkipRules()
	ic.insertInboundRules()
	ic.insertOutboundRules()
	ic.executeCommand()
}

func (ic *iptablesConstructor) insertInboundRules() {
	if ic.cfg.InboundPortsInclude == "" {
		return
	}
	ic.iptables.AppendRuleV4(log.UndefinedCommand, PreRoutingChain, "nat", "-p", "tcp", "-j", InboundChain)

	if ic.cfg.InboundPortsInclude == "*" {
		// Makes sure SSH is not redirected
		ic.iptables.AppendRuleV4(log.UndefinedCommand, InboundChain, "nat", "-p", "tcp", "--dport", "22", "-j", "RETURN")
		if ic.cfg.InboundPortsExclude != "" {
			for _, port := range split(ic.cfg.InboundPortsExclude) {
				ic.iptables.AppendRuleV4(log.UndefinedCommand, InboundChain, "nat", "-p", "tcp", "--dport", port, "-j", "RETURN")
			}
		}
		ic.iptables.AppendRuleV4(log.UndefinedCommand, InboundChain, "nat", "-p", "tcp", "-j", InboundRedirectChain)
	} else {
		for _, port := range split(ic.cfg.InboundPortsInclude) {
			ic.iptables.AppendRuleV4(
				log.UndefinedCommand, InboundChain, "nat", "-p", "tcp", "--dport", port, "-j", InboundRedirectChain,
			)
		}
	}
}

func (ic *iptablesConstructor) insertOutboundRules() {
	if ic.cfg.OutboundPortsInclude == "" {
		return
	}
	if ic.cfg.OutboundPortsInclude == "*" {
		if ic.cfg.OutboundPortsExclude != "" {
			for _, port := range split(ic.cfg.OutboundPortsExclude) {
				ic.iptables.AppendRuleV4(
					log.UndefinedCommand, OutputChain, "nat", "-p", "tcp", "--dport", port, "-j", "RETURN",
				)
			}
		}
		ic.iptables.AppendRuleV4(
			log.UndefinedCommand, OutputChain, "nat", "-p", "tcp", "-j", RedirectChain,
		)
	} else {
		for _, port := range split(ic.cfg.OutboundPortsInclude) {
			ic.iptables.AppendRuleV4(
				log.UndefinedCommand, OutputChain, "nat", "-p", "tcp", "--dport", port, "-j", RedirectChain,
			)
		}

	}
}

func (ic *iptablesConstructor) insertSkipRules() {
	ic.iptables.AppendRuleV4(log.UndefinedCommand, OutputChain, "nat", "-o", "lo", "!", "-d",
		"127.0.0.1/32", "-m", "owner", "--uid-owner", ic.cfg.ProxyUID, "-j", "RETURN")
	ic.iptables.AppendRuleV4(log.UndefinedCommand, OutputChain, "nat", "-m", "owner", "--gid-owner",
		ic.cfg.ProxyGID, "-j", "RETURN")
}

func (ic *iptablesConstructor) executeCommand() {
	commands := ic.iptables.BuildV4()
	for _, cmd := range commands {
		if len(cmd) > 1 {
			ic.dep.RunOrFail(cmd[0], cmd[1:]...)
		} else {
			ic.dep.RunOrFail(cmd[0])
		}
	}
}

func split(s string) []string {
	if s == "" {
		return nil
	}
	return filterEmpty(strings.Split(s, ","))
}

func filterEmpty(strs []string) []string {
	filtered := make([]string, 0, len(strs))
	for _, s := range strs {
		if s == "" {
			continue
		}
		filtered = append(filtered, s)
	}
	return filtered
}

func main() {
	cmd := newCommand()
	if err := cmd.Execute(); err != nil {

		os.Exit(-1)
	}
}
