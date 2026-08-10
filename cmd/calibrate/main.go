// Command calibrate turns measured metrics into thresholds.
//
// It reads a JSONL file of measurements, sweeps every threshold worth trying,
// and reports the operating point that meets a target. It never guesses: given
// data that cannot meet the target it says so and exits non-zero, because the
// alternative is a number somebody deploys believing it was measured.
//
//	calibrate sweep -in measurements.jsonl -metric mar -target-far 0.01
//
// The measurement file is one JSON object per line:
//
//	{"label":"genuine","value":0.82,"source":"session-4f2a"}
//	{"label":"attack","value":0.31,"source":"print-03.jpg"}
//
// Collecting it is the part that needs a decision this tool cannot make. See
// the notes printed by `calibrate help-data`.
package main

import (
	"bufio"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/ziad/liveness-verifier/internal/calibrate"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		// The calibrate package prefixes its own errors, so adding another
		// produces "calibrate: calibrate:" — which reads like a bug in the tool
		// at the moment somebody is already reading it because something went
		// wrong.
		msg := err.Error()
		if !strings.HasPrefix(msg, "calibrate:") {
			msg = "calibrate: " + msg
		}
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	if len(args) == 0 {
		usage(out)
		return errors.New("no command given")
	}

	switch args[0] {
	case "sweep":
		return sweepCmd(args[1:], out)
	case "help-data":
		helpData(out)
		return nil
	case "help", "-h", "--help":
		usage(out)
		return nil
	default:
		usage(out)
		return fmt.Errorf("unknown command %q", args[0])
	}
}

func usage(out io.Writer) {
	fmt.Fprint(out, `calibrate — turn measured metrics into thresholds

  sweep       read measurements, report the operating point for a target
  help-data   how to collect the measurements, and what is still undecided

`)
}

func sweepCmd(args []string, out io.Writer) error {
	fs := flag.NewFlagSet("sweep", flag.ContinueOnError)
	fs.SetOutput(out)

	var (
		in        = fs.String("in", "", "JSONL file of measurements; - for stdin")
		metric    = fs.String("metric", "", "name of the metric being calibrated, for the report")
		targetFAR = fs.Float64("target-far", -1, "hold the false accept rate at or below this")
		targetFRR = fs.Float64("target-frr", -1, "hold the false reject rate at or below this")
		lower     = fs.Bool("lower-is-genuine", false, "the metric is a distance: genuine values are small")
		curve     = fs.Bool("curve", false, "print the whole error curve, not just the operating point")
	)

	if err := fs.Parse(args); err != nil {
		return err
	}
	if *in == "" {
		return errors.New("-in is required")
	}
	if *metric == "" {
		return errors.New("-metric is required; the report is meaningless without knowing what was measured")
	}
	if *targetFAR < 0 && *targetFRR < 0 {
		return errors.New("give -target-far or -target-frr; without a target there is no operating point to choose")
	}

	ms, err := readMeasurements(*in)
	if err != nil {
		return err
	}

	dir := calibrate.HigherIsGenuine
	if *lower {
		dir = calibrate.LowerIsGenuine
	}

	res, err := calibrate.Sweep(*metric, ms, dir)
	if err != nil {
		return err
	}

	report(out, res, *curve)

	// A sample that separates perfectly is a warning, not a result. Real
	// populations overlap, so a clean split usually means the sample is small
	// or the two classes were captured in conditions that differ in some way
	// other than the one being measured.
	if res.Separable() {
		fmt.Fprintf(out, "\nNOTE  the two classes separate perfectly on this sample.\n"+
			"      That is rare in the world and common in small or tidy datasets.\n"+
			"      Treat the threshold as provisional until the sample is larger\n"+
			"      and captured the way real sessions are.\n")
	}

	if *targetFAR >= 0 {
		p, err := res.AtFAR(*targetFAR)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "\nAt FAR <= %.4f:\n", *targetFAR)
		printPoint(out, p)
	}

	if *targetFRR >= 0 {
		p, err := res.AtFRR(*targetFRR)
		if err != nil {
			return err
		}
		fmt.Fprintf(out, "\nAt FRR <= %.4f:\n", *targetFRR)
		printPoint(out, p)
	}

	return nil
}

func report(out io.Writer, r calibrate.Result, curve bool) {
	fmt.Fprintf(out, "metric      %s (%s)\n", r.Metric, r.Direction)
	fmt.Fprintf(out, "samples     %d genuine, %d attack\n", r.Genuine, r.Attacks)
	fmt.Fprintf(out, "EER         %.4f at threshold %.6f\n", r.EERRate, r.EER.Threshold)

	if !curve {
		return
	}

	fmt.Fprintf(out, "\n%12s  %8s  %8s\n", "threshold", "FAR", "FRR")
	for _, p := range r.Points {
		fmt.Fprintf(out, "%12.6f  %8.4f  %8.4f\n", p.Threshold, p.FAR, p.FRR)
	}
}

func printPoint(out io.Writer, p calibrate.Point) {
	fmt.Fprintf(out, "  threshold %.6f\n", p.Threshold)
	fmt.Fprintf(out, "  FAR       %.4f  (attacks that get through)\n", p.FAR)
	fmt.Fprintf(out, "  FRR       %.4f  (genuine people turned away)\n", p.FRR)
}

func readMeasurements(path string) ([]calibrate.Measurement, error) {
	var r io.Reader

	if path == "-" {
		r = os.Stdin
	} else {
		f, err := os.Open(path)
		if err != nil {
			return nil, fmt.Errorf("open %s: %w", path, err)
		}
		defer func() { _ = f.Close() }()
		r = f
	}

	var ms []calibrate.Measurement

	scanner := bufio.NewScanner(r)
	// Measurement lines are short, but a truncated read would silently drop
	// samples and change the answer.
	scanner.Buffer(make([]byte, 0, 64<<10), 1<<20)

	for line := 1; scanner.Scan(); line++ {
		text := strings.TrimSpace(scanner.Text())
		if text == "" || strings.HasPrefix(text, "#") {
			continue
		}

		var m calibrate.Measurement
		if err := json.Unmarshal([]byte(text), &m); err != nil {
			return nil, fmt.Errorf("%s line %d: %w", path, line, err)
		}
		ms = append(ms, m)
	}
	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}

	if len(ms) == 0 {
		return nil, fmt.Errorf("%s holds no measurements", path)
	}
	return ms, nil
}

func helpData(out io.Writer) {
	fmt.Fprint(out, `Collecting the measurements

The genuine class you can collect yourself, today. Run the demo with
LV_LOG_LEVEL=debug and every frame writes a line like:

  {"msg":"frame measured","ear":0.27,"mar":0.31,"liveness":0.006,...}

Pull the metric you are calibrating out of those lines and label it "genuine".
Twenty sessions from a handful of people in the light they will actually use is
worth more than a thousand frames of one person at a desk.

The attack class is the part that needs a decision, not a tool. It means
deliberately presenting attacks to the camera and labelling them:

  - a printed photograph of a face, matte and glossy
  - the same face on a phone or laptop screen
  - a photograph on a screen, photographed again

None of it belongs in this repository. Captures of real people are exactly the
data this system exists to protect, and a repository is not where they go.
Keep them outside it and point -in at the file.

What is still undecided, and why this tool cannot decide it

  Open Question #2 — the target FAR and FRR.

    How many attacks getting through is acceptable, against how many genuine
    people turned away. That trade is a business decision: a bank and a
    building-entry kiosk want opposite ends of it, and neither answer is more
    correct than the other.

  Open Question #3 — the dataset.

    Nothing here can be measured without both classes. This tool refuses to
    produce a threshold from one, because a threshold that has never seen an
    attack is a threshold nobody should ship.

Both are recorded in SPEC.md. Answer them and this becomes one command.
`)
}
