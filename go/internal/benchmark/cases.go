// Package benchmark defines small, versioned tasks with host-owned checks.
package benchmark

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"syscall"
)

const Version = 1

type Case struct {
	Name   string
	Prompt string
	Files  map[string]string
}

func Cases() []Case {
	return []Case{
		{Name: "data-summary", Prompt: "Read orders.csv and write summary.json with exactly the keys orders (row count), totalCents (sum of quantity * priceCents), and byRegion (the same totals grouped by region). Use integer arithmetic. Do not use the network or install packages.", Files: map[string]string{
			"orders.csv": "region,quantity,priceCents\nnorth,2,1250\nsouth,1,399\nnorth,3,50\nwest,0,999\nsouth,2,399\n",
		}},
		{Name: "normalize-tags", Prompt: "Fix normalize_tags in tags.py. Strip surrounding whitespace, lowercase tags, omit empty tags, and deduplicate while preserving first-occurrence order. Do not change the input list. None input means an empty list. Raise TypeError for a non-list input or a non-string element. Use Python's standard library only; do not fetch or install anything.", Files: map[string]string{
			"tags.py": "def normalize_tags(tags):\n    return sorted(set(tags))\n",
		}},
	}
}

func VerifySummary(root *os.Root) error {
	f, err := root.OpenFile("summary.json", os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Size() > 65536 {
		return fmt.Errorf("summary must be a regular JSON file under 64 KiB")
	}
	var got struct {
		Orders  int            `json:"orders"`
		Total   int            `json:"totalCents"`
		Regions map[string]int `json:"byRegion"`
	}
	d := json.NewDecoder(io.LimitReader(f, 65537))
	d.DisallowUnknownFields()
	if err := d.Decode(&got); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("unexpected trailing data")
	}
	if got.Orders != 5 || got.Total != 3847 || len(got.Regions) != 3 || got.Regions["north"] != 2650 || got.Regions["south"] != 1197 {
		return fmt.Errorf("incorrect order totals: %+v", got)
	}
	if west, ok := got.Regions["west"]; !ok || west != 0 {
		return fmt.Errorf("zero-quantity region missing")
	}
	return nil
}

// The test stays outside the agent workspace and is passed to a confined verifier.
const VerifyTags = `import runpy, sys
f = runpy.run_path(sys.argv[1])["normalize_tags"]
assert f(None) == []
assert f([]) == []
values = [" Go ", "RUST", "go", "", "  ", "Rust", "Python"]
before = values.copy()
assert f(values) == ["go", "rust", "python"]
assert values == before
assert f([" Z ", "a", "z"]) == ["z", "a"]
for bad in ["go", 7, {}, ["go", None], ["go", 3]]:
    try:
        f(bad)
    except TypeError:
        pass
    else:
        raise AssertionError("expected TypeError")
print("checks passed")
`
