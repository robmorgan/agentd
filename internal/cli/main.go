// Package cli is the agent command-line client: every command, the
// session picker, and attach with its overlay. It talks to agentd only
// through the agent protocol, locally over the daemon's Unix socket or
// remotely over QUIC, and never opens the daemon's state itself.
package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sort"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"

	"github.com/robmorgan/agentd/internal/config"
	"github.com/robmorgan/agentd/internal/paths"
	"github.com/robmorgan/agentd/internal/transport"
)

// Version is the client version, reported by `agent --version` and
// `agent daemon info`. Builds set it (and agentd's) from one place.
var Version = "0.1.0"

const rootIntro = `A local multi-session coding workflow for daemon-backed agents.

Core flows:
  Start a session      agent new fix-flaky-tests
  Inspect sessions     agent list
  Reconnect live PTY   agent attach <name>
  Review its work      agent diff <name>
  Run somewhere else   agent new --cwd ../wt/fix fix`

const rootExamples = `Examples:
  agent new add-health-checks
  agent list
  agent status <name>
  agent daemon info

Use ` + "`agent <command> --help`" + ` for command-specific details.`

// usageError is a command line that does not parse. It exits with status 2.
type usageError struct{ err error }

func (e usageError) Error() string { return e.err.Error() }

// Main runs the agent CLI with args (without the program name) and returns
// the exit status.
func Main(args []string) (status int) {
	a := &app{}
	defer func() {
		if r := recover(); r != nil {
			// Put the terminal back before the panic is printed.
			writeOut([]byte(attachRestoreSequence))
			panic(r)
		}
	}()
	root := a.rootCommand()
	root.SetArgs(args)
	err := root.Execute()
	if a.client != nil {
		err = a.client.explain(err)
		a.client.close()
	}
	if err == nil {
		return 0
	}
	var usage usageError
	if errors.As(err, &usage) || !a.ran {
		fmt.Fprintf(os.Stderr, "error: %v\n\nFor more information, try '--help'.\n", err)
		return 2
	}
	fmt.Fprintf(os.Stderr, "Error: %v\n", err)
	return 1
}

// app is one invocation of the CLI.
type app struct {
	paths  *paths.AppPaths
	host   string // --host
	client *client
	ran    bool // parsing succeeded and a command started
}

// setup prepares the runtime root. It runs before every command.
func (a *app) setup() error {
	p, err := paths.Discover()
	if err != nil {
		return err
	}
	if err := p.EnsureLayout(); err != nil {
		return err
	}
	if err := config.WriteDefault(p.Config); err != nil {
		return err
	}
	a.paths = p
	return nil
}

// connect chooses the daemon for a command and makes sure it is there.
// sessionArgs may carry a host/ prefix, which is stripped. daemonCommand is
// the `agent daemon` subcommand, if that is what runs. before runs once the
// target is known but before a daemon is started, so a command can reject
// bad arguments first.
func (a *app) connect(sessionArgs []*string, daemonCommand string, before func(*client) error) (*client, error) {
	prefixed, err := takeSessionHost(sessionArgs)
	if err != nil {
		return nil, err
	}
	name := a.host
	switch {
	case name != "" && prefixed != "" && name != prefixed:
		return nil, fmt.Errorf("--host %s conflicts with the session address's host `%s`", name, prefixed)
	case name == "":
		name = prefixed
	}
	switch name {
	case localHostName:
		name = ""
	case autoHostName:
		return nil, errors.New("`--host auto` only chooses a host for `agent new`")
	}
	c := &client{paths: a.paths}
	if name != "" {
		host, err := transport.LookupHost(a.paths.HostsPath(), name)
		if err != nil {
			return nil, err
		}
		c.host = &host
	}
	a.client = c
	if before != nil {
		if err := before(c); err != nil {
			return nil, err
		}
	}
	if err := c.prepareDaemon(daemonCommand); err != nil {
		return nil, err
	}
	return c, nil
}

// takeSessionHost strips a host/ prefix from session arguments and returns
// the host it names. Session names cannot contain '/', so the prefix is
// unambiguous.
func takeSessionHost(ids []*string) (string, error) {
	host := ""
	for _, id := range ids {
		if id == nil {
			continue
		}
		prefix, rest, ok := strings.Cut(*id, "/")
		if !ok {
			continue
		}
		if prefix == "" || rest == "" {
			return "", fmt.Errorf("`%s` is not a session address (expected HOST/SESSION)", *id)
		}
		if host != "" && host != prefix {
			return "", fmt.Errorf("session addresses name different hosts: `%s` and `%s`", host, prefix)
		}
		host = prefix
		*id = rest
	}
	return host, nil
}

// command builds one subcommand. run gets the parsed positional arguments.
func (a *app) command(use, short string, args cobra.PositionalArgs, run func(args []string) error) *cobra.Command {
	return &cobra.Command{
		Use:   use,
		Short: short,
		Args:  args,
		RunE: func(cmd *cobra.Command, args []string) error {
			a.ran = true
			if err := a.setup(); err != nil {
				return err
			}
			return run(args)
		},
	}
}

// group builds a command that only holds subcommands.
func group(use, short, intro, after string, subs ...*cobra.Command) *cobra.Command {
	cmd := &cobra.Command{
		Use:   use,
		Short: short,
		Args:  noArgs,
		RunE: func(cmd *cobra.Command, args []string) error {
			return usageError{fmt.Errorf("'%s' requires a subcommand", cmd.CommandPath())}
		},
	}
	cmd.Annotations = map[string]string{"intro": intro, "after": after}
	cmd.AddCommand(subs...)
	return cmd
}

func visibleAlias(cmd *cobra.Command, alias string, hidden ...string) *cobra.Command {
	cmd.Aliases = append([]string{alias}, hidden...)
	if cmd.Annotations == nil {
		cmd.Annotations = map[string]string{}
	}
	cmd.Annotations["alias"] = alias
	return cmd
}

func argsError(check cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := check(cmd, args); err != nil {
			return usageError{err}
		}
		return nil
	}
}

var (
	noArgs      = argsError(cobra.NoArgs)
	oneArg      = argsError(cobra.ExactArgs(1))
	twoArgs     = argsError(cobra.ExactArgs(2))
	optionalArg = argsError(cobra.MaximumNArgs(1))
)

func (a *app) rootCommand() *cobra.Command {
	root := &cobra.Command{
		Use:           "agent",
		Short:         "Run, inspect, and review local coding sessions",
		Version:       Version,
		Args:          noArgs,
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			a.ran = true
			if err := a.setup(); err != nil {
				return err
			}
			return a.runRuntime("")
		},
	}
	root.Annotations = map[string]string{"intro": rootIntro, "after": rootExamples}
	root.CompletionOptions.DisableDefaultCmd = true
	root.PersistentFlags().StringVar(&a.host, "host", "", "Talk to the agentd on a remote host (see `agent host add`)")
	root.PersistentFlags().Lookup("host").Annotations = map[string][]string{"value": {"NAME"}}
	root.Flags().SortFlags = false
	root.PersistentFlags().SortFlags = false
	root.Flags().BoolP("help", "h", false, "Print help")
	root.Flags().BoolP("version", "V", false, "Print version")
	root.SetVersionTemplate("agent {{.Version}}\n")
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usageError{err} })
	root.SetHelpFunc(func(cmd *cobra.Command, _ []string) { writeHelp(os.Stdout, cmd) })
	root.SetHelpCommand(&cobra.Command{Hidden: true})
	root.AddCommand(a.commands()...)
	return root
}

// Help. It follows the agent CLI's long-standing layout: a header for the
// root, usage, commands (with visible aliases), options, then notes or
// examples.

const (
	helpHeading = "\x1b[36;1m"
	helpUsage   = "\x1b[33;1m"
	helpLiteral = "\x1b[1m\x1b[38;2;95;251;255m"
)

func writeHelp(w io.Writer, cmd *cobra.Command) {
	color := w == io.Writer(os.Stdout) && stdoutIsTerminal()
	style := func(s, text string) string {
		if !color {
			return text
		}
		return s + text + ansiReset
	}
	var b strings.Builder
	switch {
	case !cmd.HasParent():
		b.WriteString(header() + "\n" + cmd.Annotations["intro"] + "\n\n")
		fmt.Fprintf(&b, "agent %s\n\n%s\n\n", Version, cmd.Short)
	case cmd.Annotations["intro"] != "":
		fmt.Fprintf(&b, "%s\n%s\n\n", cmd.CommandPath(), cmd.Annotations["intro"])
	default:
		fmt.Fprintf(&b, "%s\n\n", cmd.Short)
	}
	fmt.Fprintf(&b, "%s %s\n", style(helpUsage, "Usage:"), usageLine(cmd))

	var subs []*cobra.Command
	for _, sub := range cmd.Commands() {
		if !sub.Hidden {
			subs = append(subs, sub)
		}
	}
	if len(subs) > 0 {
		width := 0
		for _, sub := range subs {
			width = max(width, len(sub.Name()))
		}
		fmt.Fprintf(&b, "\n%s\n", style(helpHeading, "Commands:"))
		for _, sub := range subs {
			line := sub.Short
			if alias := sub.Annotations["alias"]; alias != "" {
				line += " [aliases: " + alias + "]"
			}
			fmt.Fprintf(&b, "  %s%s  %s\n", style(helpLiteral, sub.Name()), strings.Repeat(" ", width-len(sub.Name())), line)
		}
	}

	if args := cmd.Annotations["args"]; args != "" {
		fmt.Fprintf(&b, "\n%s\n", style(helpHeading, "Arguments:"))
		for _, arg := range strings.Split(args, "\n") {
			fmt.Fprintf(&b, "  %s\n", strings.TrimRight(arg, " "))
		}
	}

	type option struct{ name, usage string }
	var options []option
	add := func(f *pflag.Flag) {
		if f.Hidden {
			return
		}
		name := "    "
		if f.Shorthand != "" {
			name = "-" + f.Shorthand + ", "
		}
		name += "--" + f.Name
		if values := f.Annotations["value"]; len(values) > 0 {
			name += " <" + values[0] + ">"
		}
		options = append(options, option{name, f.Usage})
	}
	cmd.NonInheritedFlags().VisitAll(add)
	cmd.InheritedFlags().VisitAll(add)
	sort.SliceStable(options, func(i, j int) bool {
		rank := func(o option) int {
			switch {
			case strings.Contains(o.name, "--help"):
				return 2
			case strings.Contains(o.name, "--version"):
				return 3
			}
			return 1
		}
		return rank(options[i]) < rank(options[j])
	})
	width := 0
	for _, o := range options {
		width = max(width, len(o.name))
	}
	fmt.Fprintf(&b, "\n%s\n", style(helpHeading, "Options:"))
	for _, o := range options {
		fmt.Fprintf(&b, "%s\n", strings.TrimRight(fmt.Sprintf("  %s%s  %s", style(helpLiteral, o.name), strings.Repeat(" ", width-len(o.name)), o.usage), " "))
	}
	if after := cmd.Annotations["after"]; after != "" {
		fmt.Fprintf(&b, "\n%s\n", after)
	}
	io.WriteString(w, b.String())
}

func usageLine(cmd *cobra.Command) string {
	path := cmd.CommandPath()
	if cmd.HasAvailableSubCommands() {
		if cmd.Runnable() && !cmd.HasParent() {
			return path + " [OPTIONS] [COMMAND]"
		}
		return path + " <COMMAND>"
	}
	if u := cmd.Annotations["usage"]; u != "" {
		return path + " " + u
	}
	return path + " [OPTIONS]"
}

func init() {
	cobra.EnableCommandSorting = false
}
