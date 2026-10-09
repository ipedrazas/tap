package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/ipedrazas/tap/pkg/inventory"
)

type kubeFlags struct{ kubeconfig, context *string }

func addKubeFlags(fs *flag.FlagSet) kubeFlags {
	return kubeFlags{
		kubeconfig: fs.String("kubeconfig", "", "path to kubeconfig (default: $KUBECONFIG or ~/.kube/config)"),
		context:    fs.String("context", "", "kubeconfig context (default: current)"),
	}
}

func (k kubeFlags) client() (kubernetes.Interface, error) {
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	if *k.kubeconfig != "" {
		rules.ExplicitPath = *k.kubeconfig
	}
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{CurrentContext: *k.context}).ClientConfig()
	if err != nil {
		return nil, err
	}
	cfg.Timeout = 15 * time.Second
	return kubernetes.NewForConfig(cfg)
}

func cmdAgentLs(args []string) error {
	fs := newFlagsPlain("agent ls")
	kf := addKubeFlags(fs)
	out := fs.String("o", "table", "output: table, wide or json")
	if err := fs.Parse(args); err != nil {
		return err
	}
	c, err := kf.client()
	if err != nil {
		return err
	}
	agents, err := inventory.List(context.Background(), c)
	if err != nil {
		return err
	}
	switch *out {
	case "json":
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(agents)
	case "table", "wide":
	default:
		return fmt.Errorf("-o must be table, wide or json")
	}
	if len(agents) == 0 {
		fmt.Println("no agents running")
		return nil
	}
	now := time.Now()
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	if *out == "wide" {
		fmt.Fprintln(tw, "NAME\tSTATUS\tREADY\tVERSION\tMODEL\tTOOLS\tMCP\tEGRESS\tRESTARTS\tAGE\tBUNDLE\tOWNER\tURL")
	} else {
		fmt.Fprintln(tw, "NAME\tSTATUS\tREADY\tVERSION\tMODEL\tTOOLS\tMCP\tRESTARTS\tAGE\tURL")
	}
	for _, a := range agents {
		ready := fmt.Sprintf("%d/%d", a.Ready, a.Desired)
		age := inventory.Age(a.Created, now)
		if *out == "wide" {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%s\t%s\t%s\t%s\n", a.Name, a.Status, ready, a.Version, a.Model, a.ScriptTools(), a.MCPTools(), len(a.Egress), a.Restarts, age, inventory.ShortDigest(a.Bundle), a.Owner, a.URL)
		} else {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%d\t%d\t%d\t%s\t%s\n", a.Name, a.Status, ready, a.Version, a.Model, a.ScriptTools(), a.MCPTools(), a.Restarts, age, a.URL)
		}
	}
	return tw.Flush()
}

func cmdAgentGet(args []string) error {
	fs := newFlagsPlain("agent get")
	kf := addKubeFlags(fs)
	out := fs.String("o", "text", "output: text or json")
	name, err := parse(fs, args)
	if err != nil {
		return err
	}
	c, err := kf.client()
	if err != nil {
		return err
	}
	a, err := inventory.Get(context.Background(), c, name)
	if err != nil {
		return err
	}
	if *out == "json" {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		return enc.Encode(a)
	}
	now := time.Now()
	none := func(xs []string) string {
		if len(xs) == 0 {
			return "none"
		}
		return strings.Join(xs, ", ")
	}
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	for _, row := range [][2]string{
		{"Name", a.Name},
		{"Description", a.Description},
		{"Owner", a.Owner},
		{"Version", a.Version},
		{"Status", fmt.Sprintf("%s (%d/%d ready, %d restarts)", a.Status, a.Ready, a.Desired, a.Restarts)},
		{"URL", a.URL},
		{"Model route", a.Model},
		{"Tools", none(a.Tools)},
		{"MCP servers", none(a.MCP)},
		{"Egress", none(a.Egress)},
		{"Namespace", a.Namespace},
		{"Bundle", a.Bundle},
		{"Runner", a.Runner},
		{"Harness", a.Harness},
		{"Age", inventory.Age(a.Created, now)},
	} {
		fmt.Fprintf(tw, "%s:\t%s\n", row[0], row[1])
	}
	tw.Flush()
	if len(a.Pods) > 0 {
		fmt.Println("\nPods:")
		tw = tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "  NAME\tPHASE\tREADY\tRESTARTS\tNODE\tAGE\tREASON")
		for _, p := range a.Pods {
			fmt.Fprintf(tw, "  %s\t%s\t%v\t%d\t%s\t%s\t%s\n", p.Name, p.Phase, p.Ready, p.Restarts, p.Node, inventory.Age(p.Started, now), p.Reason)
		}
		tw.Flush()
	}
	return nil
}
