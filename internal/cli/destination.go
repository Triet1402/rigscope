package cli

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"strconv"
	"strings"

	"github.com/Triet1402/rigscope/internal/collector/enroll"
	"github.com/Triet1402/rigscope/internal/config"
)

// destinationPlan is what `install --hub URL --token GRANT` does to an
// existing collector. A brand-new collector has nothing to lose: install
// stages the grant and the service redeems it when it starts, so an install
// still completes while the hub is unreachable. An existing one is handled
// before anything is written:
//
//   - already enrolled with that hub: the grant is redeemed during install
//     and replaces the stored credential. The local credential file cannot
//     tell whether the hub still honours it (the machine may have been
//     removed there), so it is never a reason to discard the grant. When the
//     hub refuses, the collector keeps its credential and install says so;
//   - the hub is this machine's own: the collector keeps its direct local
//     address instead of leaving through a proxy and coming back;
//   - enrolled with another hub: moving is confirmed first, and the grant is
//     redeemed before the destination changes, so a refused grant leaves the
//     collector sending to the hub it already works with.
type destinationPlan struct {
	// ownHub: the URL named this machine's own hub, and the destination was
	// rewritten to the hub's local address.
	ownHub bool
	// reenroll: the collector already holds a credential for the
	// destination, so the grant is redeemed during install to replace it.
	reenroll bool
	// refused is the hub's answer when a reenroll redemption failed; the
	// collector keeps the credential it has.
	refused error
	// moveFrom is the destination the collector is enrolled with today, when
	// this install moves it to another hub.
	moveFrom string
	// enrolled: the grant was redeemed during install (a move).
	enrolled  bool
	machineID string
}

// stagesGrant reports whether install should still stage the grant for the
// collector service to redeem.
func (p destinationPlan) stagesGrant() bool {
	return !p.reenroll && !p.enrolled
}

// restartsCollector reports whether install replaced the collector's
// credential, so a running collector must restart to use it.
func (p destinationPlan) restartsCollector() bool {
	return p.reenroll && p.enrolled
}

// planDestination decides how --hub applies to this installation. It runs
// before anything is written and asks before moving an enrolled collector.
// hubURL and hubCA may be rewritten to the hub's local address.
func (d Deps) planDestination(paths config.Paths, existing bool, prior, cfg *config.Config, hubURL, hubCA *string, assumeYes bool) (destinationPlan, error) {
	var plan destinationPlan
	if *hubURL == "" || !existing {
		return plan, nil
	}
	if cfg.Components.Hub.Enabled && isOwnHubURL(*hubURL, cfg.Components.Hub) {
		if local, ok, _ := localHubDestination(cfg.Components.Hub); ok {
			*hubURL, *hubCA = local.URL, local.CAFile
			cfg.Components.Collector.Destinations = []config.Destination{local}
			plan.ownHub = true
		}
	}
	enrolled, err := collectorHasCredential(paths, *hubURL)
	if err != nil {
		return plan, err
	}
	if enrolled {
		plan.reenroll = true
		return plan, nil
	}
	if !prior.Components.Collector.Enabled || len(prior.Components.Collector.Destinations) != 1 {
		return plan, nil
	}
	current := prior.Components.Collector.Destinations[0].URL
	if current == *hubURL {
		return plan, nil
	}
	working, err := collectorHasCredential(paths, current)
	if err != nil {
		return plan, err
	}
	if !working {
		// The collector never enrolled with its current destination, so it
		// is not sending anywhere: nothing is lost by pointing it elsewhere.
		return plan, nil
	}
	plan.moveFrom = current
	fmt.Fprintf(d.Stderr, "%s This collector sends to %s.\n   Moving it to %s stops that hub from receiving this machine's data.\n",
		d.errStyle(styleCyan+";"+styleBold, "::"), d.errStyle(styleBold, hubAddress(current)), d.errStyle(styleBold, hubAddress(*hubURL)))
	if assumeYes {
		return plan, nil
	}
	ok, err := d.confirm("Move it?")
	if errors.Is(err, errNoTerminal) {
		return plan, fmt.Errorf("this collector already sends to %s and there is no terminal to confirm the move on; run again with --yes to move it", hubAddress(current))
	}
	if err != nil {
		return plan, err
	}
	if !ok {
		return plan, errCancelled
	}
	return plan, nil
}

// redeemGrant redeems the grant during install when the collector already
// holds a credential: before the destination changes for a move, or to
// replace the credential for its current hub. A refused move fails the
// install with nothing changed; a refused re-enrollment keeps the existing
// credential and is reported afterwards.
func (d Deps) redeemGrant(ctx context.Context, paths config.Paths, plan *destinationPlan, hubURL, hubCA, grant string) error {
	if plan.moveFrom == "" && !plan.reenroll {
		return nil
	}
	store, err := openCollectorStore(d, paths.StateDir)
	if err != nil {
		return err
	}
	result, err := d.enroll(ctx, enroll.Options{Store: store, HubURL: hubURL, Grant: grant, CAFile: hubCA})
	if err != nil && plan.reenroll {
		plan.refused = err
		return nil
	}
	if err != nil {
		return fmt.Errorf("%s did not accept the grant (%v)\n  nothing changed: this collector still sends to %s", hubAddress(hubURL), err, hubAddress(plan.moveFrom))
	}
	plan.enrolled = true
	plan.machineID = result.MachineID
	return nil
}

// printDestinationPlan reports what --hub did to the collector.
func printDestinationPlan(d Deps, plan destinationPlan, hubURL string) {
	label := d.out(styleGreen, "collector:")
	if plan.ownHub {
		fmt.Fprintf(d.Stdout, "  %s --hub is this machine's own hub; the collector uses its direct local address %s\n", label, hubURL)
	}
	switch {
	case plan.refused != nil:
		d.warnf(d.Stdout, "the grant was not redeemed with %s (%v)\n"+
			"  This collector keeps its existing credential for that hub. If this machine is listed in the hub, it is\n"+
			"  already connected: one installation runs one collector, so there is nothing to add. If it is missing\n"+
			"  (removed there), create a new grant in the hub under Administration > Collectors and run the command again.",
			hubAddress(hubURL), plan.refused)
	case plan.reenroll && plan.enrolled:
		fmt.Fprintf(d.Stdout, "  %s enrolled again with %s (machine %s); the new credential replaces the old one\n", label, hubAddress(hubURL), plan.machineID)
	case plan.enrolled:
		fmt.Fprintf(d.Stdout, "  %s moved from %s to %s (machine %s)\n", label, hubAddress(plan.moveFrom), hubAddress(hubURL), plan.machineID)
	}
}

// isOwnHubURL reports whether a collector destination names this machine's
// hub: its configured origin, or a loopback address on its port.
func isOwnHubURL(raw string, hub config.Hub) bool {
	target, err := url.Parse(raw)
	if err != nil || target.Host == "" {
		return false
	}
	if hub.Origin != "" {
		if origin, err := url.Parse(hub.Origin); err == nil && strings.EqualFold(authority(origin), authority(target)) {
			return true
		}
	}
	host := strings.Trim(target.Hostname(), "[]")
	if ip := net.ParseIP(host); (ip != nil && ip.IsLoopback()) || strings.EqualFold(host, "localhost") {
		return target.Port() == strconv.Itoa(hub.Port)
	}
	return false
}

// authority is host:port with the scheme's default port filled in, so that
// https://h and wss://h:443 compare equal.
func authority(u *url.URL) string {
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https", "wss":
			port = "443"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(strings.ToLower(u.Hostname()), port)
}

// hubAddress shows a collector destination as the hub address an operator
// types: wss://host/api/collector/connect becomes https://host.
func hubAddress(destination string) string {
	parsed, err := url.Parse(destination)
	if err != nil || parsed.Host == "" {
		return destination
	}
	switch parsed.Scheme {
	case "ws":
		parsed.Scheme = "http"
	case "wss":
		parsed.Scheme = "https"
	}
	return parsed.Scheme + "://" + parsed.Host
}
