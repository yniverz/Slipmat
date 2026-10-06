// SPDX-License-Identifier: GPL-3.0-or-later

// Command slipmat makes this computer a rekordbox "Link Export" source for
// Pioneer/AlphaTheta players on a Pro DJ Link network.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/netip"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/yniverz/slipmat/internal/device"
	"github.com/yniverz/slipmat/internal/logx"
	"github.com/yniverz/slipmat/internal/monitor"
	"github.com/yniverz/slipmat/internal/netif"
	"github.com/yniverz/slipmat/internal/prolink"
)

var version = "0.1.0-dev"

const usage = `slipmat — act as a rekordbox Link Export source for Pro DJ Link players

Usage:
  slipmat interfaces [-probe]       list network interfaces (and which see Pro DJ Link traffic)
  slipmat monitor    [flags]        passively log Pro DJ Link traffic (sends nothing)
  slipmat decode     [flags] FILE   decode a tcpdump/Wireshark capture (.pcap/.pcapng)
  slipmat serve      [flags]        appear on the link as a rekordbox source
  slipmat version

Common flags:
  -i, -interface NAME   interface to use: OS name (en13), label ("USB"), or IP
  -pick                 ignore the saved interface and choose again
  -v / -vv              debug / trace logging (trace adds hex dumps of every packet)

Run "slipmat <command> -h" for command flags.
`

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	cmd, args := os.Args[1], os.Args[2:]
	var err error
	switch cmd {
	case "interfaces", "ifs":
		err = cmdInterfaces(args)
	case "monitor":
		err = cmdMonitor(args)
	case "decode":
		err = cmdDecode(args)
	case "serve":
		err = cmdServe(args)
	case "version", "-version", "--version":
		fmt.Println("slipmat", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "unknown command %q\n\n%s", cmd, usage)
		os.Exit(2)
	}
	if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, flag.ErrHelp) {
		fmt.Fprintln(os.Stderr, "error:", err)
		os.Exit(1)
	}
}

// common holds flags shared by the network commands.
type common struct {
	iface   string
	pick    bool
	verbose int
}

func (c *common) register(fs *flag.FlagSet) {
	fs.StringVar(&c.iface, "interface", "", "network interface (OS name, label or IP)")
	fs.StringVar(&c.iface, "i", "", "shorthand for -interface")
	fs.BoolVar(&c.pick, "pick", false, "choose the interface again instead of using the saved one")
	fs.Func("v", "debug logging (repeat or use -vv for trace)", func(string) error { c.verbose++; return nil })
	fs.Func("vv", "trace logging with hex dumps", func(string) error { c.verbose += 2; return nil })
}

// parse parses args, accepting bare -v / -vv.
func parse(fs *flag.FlagSet, args []string) error {
	// flag.Func requires a value; rewrite bare -v/-vv to -v=1 forms.
	out := make([]string, 0, len(args))
	for _, a := range args {
		switch a {
		case "-v", "--v":
			a = "-v=1"
		case "-vv", "--vv":
			a = "-vv=1"
		case "-vvv":
			a = "-vv=1"
		}
		out = append(out, a)
	}
	return fs.Parse(out)
}

func signalContext() (context.Context, context.CancelFunc) {
	return signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func (c *common) chooseInterface(ctx context.Context) (netif.Interface, error) {
	log := logx.Component("net")
	if c.pick && c.iface == "" {
		ifs, err := netif.List()
		if err != nil {
			return netif.Interface{}, err
		}
		seen, err := netif.Probe(ctx, ifs, 3*time.Second)
		if err != nil {
			return netif.Interface{}, err
		}
		ifc, err := netif.Menu(os.Stdin, os.Stderr, ifs, seen)
		if err != nil {
			return ifc, err
		}
		if err := netif.SaveConfig(netif.Config{Interface: ifc.Name}); err != nil {
			log.Warn("could not save interface choice", "err", err)
		}
		return ifc, nil
	}
	ch := netif.Chooser{
		Spec:        c.iface,
		Interactive: isTerminal(os.Stdin),
		In:          os.Stdin,
		Out:         os.Stderr,
		ProbeFor:    3 * time.Second,
		Log:         func(f string, a ...any) { log.Info(fmt.Sprintf(f, a...)) },
	}
	ifc, err := ch.Choose(ctx)
	if err != nil {
		return ifc, err
	}
	if ifc.Wireless {
		log.Warn("the chosen interface looks like Wi-Fi; CDJs need a wired connection", "interface", ifc.String())
	}
	return ifc, nil
}

func cmdInterfaces(args []string) error {
	fs := flag.NewFlagSet("interfaces", flag.ContinueOnError)
	probe := fs.Bool("probe", true, "listen 3 s for Pro DJ Link keep-alives on each interface")
	guess := fs.Bool("guess", false, "print only the name of the most likely CDJ interface (for scripts)")
	if err := fs.Parse(args); err != nil {
		return err
	}
	ifs, err := netif.List()
	if err != nil {
		return err
	}
	var seen map[string][]netif.Sighting
	if *probe {
		fmt.Fprintln(os.Stderr, "listening 3s for Pro DJ Link devices...")
		ctx, cancel := signalContext()
		defer cancel()
		seen, err = netif.Probe(ctx, ifs, 3*time.Second)
		if err != nil {
			fmt.Fprintln(os.Stderr, "probe skipped:", err)
		}
	}
	if *guess {
		ifc, err := netif.Guess(ifs, seen, netif.LoadConfig().Interface)
		if err != nil {
			return err
		}
		fmt.Println(ifc.Name)
		return nil
	}
	netif.Table(os.Stdout, ifs, seen)
	if saved := netif.LoadConfig().Interface; saved != "" {
		fmt.Printf("\nsaved choice: %s\n", saved)
	}
	return nil
}

func cmdMonitor(args []string) error {
	fs := flag.NewFlagSet("monitor", flag.ContinueOnError)
	var c common
	c.register(fs)
	all := fs.Bool("all", false, "log every packet (including beats, positions and unchanged status)")
	every := fs.Duration("summary", 30*time.Second, "interval between peer-table summaries (0 = off)")
	if err := parse(fs, args); err != nil {
		return err
	}
	logx.Stderr(c.verbose)
	ctx, cancel := signalContext()
	defer cancel()
	ifc, err := c.chooseInterface(ctx)
	if err != nil {
		return err
	}
	conns, err := monitor.Listen(ctx, ifc)
	if err != nil {
		return err
	}
	log := logx.Component("monitor")
	log.Info("monitoring Pro DJ Link traffic (passive; Ctrl-C to stop)", "interface", ifc.String())
	obs := monitor.NewObserver(logx.Component("link"))
	obs.Verbose = *all
	obs.Self = ifc.Prefix.Addr()
	monitor.Run(ctx, conns, obs, log, *every)
	obs.Summary()
	return nil
}

func cmdDecode(args []string) error {
	fs := flag.NewFlagSet("decode", flag.ContinueOnError)
	verbose := 0
	fs.Func("v", "more output", func(string) error { verbose++; return nil })
	fs.Func("vv", "trace with hex dumps", func(string) error { verbose += 2; return nil })
	quiet := fs.Bool("changes", false, "only log status/beat packets when they change (like monitor)")
	host := fs.String("host", "", "only show packets to/from this IP")
	dups := fs.Bool("dups", false, "keep identical packets sent within 2 ms (disables pktap duplicate filtering)")
	if err := parse(fs, args); err != nil {
		return err
	}
	if fs.NArg() != 1 {
		return errors.New("usage: slipmat decode [flags] FILE.pcap")
	}
	logx.Stderr(verbose)
	d := &monitor.CaptureDecoder{Log: logx.Component("decode")}
	d.Obs = monitor.NewObserver(logx.Component("link"))
	d.Obs.Verbose = !*quiet
	d.KeepDuplicates = *dups
	if *host != "" {
		a, err := netip.ParseAddr(*host)
		if err != nil {
			return fmt.Errorf("-host: %w", err)
		}
		d.Filter = a
	}
	return d.DecodeFile(fs.Arg(0))
}

func cmdServe(args []string) error {
	fs := flag.NewFlagSet("serve", flag.ContinueOnError)
	var c common
	c.register(fs)
	name := fs.String("name", "Slipmat", "device name in Pro DJ Link packets (max 20 bytes)")
	host := fs.String("host-name", "", "computer name shown on the players (default: Slipmat)")
	devNum := fs.Uint("device-number", 17, "device number to claim (rekordbox uses 17)")
	force := fs.Bool("force", false, "start even if another rekordbox source is on the link")
	if err := parse(fs, args); err != nil {
		return err
	}
	logx.Stderr(c.verbose)
	if *devNum == 0 || *devNum > 127 {
		return errors.New("-device-number must be between 1 and 127")
	}
	if len(*name) > 20 {
		return errors.New("-name must be at most 20 bytes")
	}
	if *host == "" {
		*host = "Slipmat"
	}
	ctx, cancel := signalContext()
	defer cancel()
	ifc, err := c.chooseInterface(ctx)
	if err != nil {
		return err
	}
	log := logx.Component("device")
	dev, err := device.New(device.Config{
		Interface:    ifc,
		Name:         *name,
		Host:         strings.TrimSpace(*host),
		DeviceNumber: uint8(*devNum),
		Media:        prolink.MediaInfo{Name: *host, Settings: true}, // Settings mirrors rekordbox 7 [RB7]
		Force:        *force,
	}, log)
	if err != nil {
		return err
	}
	if err := dev.Run(ctx); err != nil {
		return err
	}
	dev.Observer().Summary()
	return nil
}
