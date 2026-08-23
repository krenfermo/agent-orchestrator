package integration

import "testing"

// The automatic rule is the one place this package decides something on a
// person's behalf, so the table is written as "what must resolve" and "what
// must NOT", and the second half is the important half: every case there is a
// conflict where resolving it would mean choosing between two intentions.
func TestAppendOnlyResolution(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name               string
		base, sideA, sideB string
		want               string
		wantOK             bool
	}{
		{
			name:   "both sides appended distinct lines to a shared list",
			base:   "alpha\nbravo\n",
			sideA:  "alpha\nbravo\ncharlie\n",
			sideB:  "alpha\nbravo\ndelta\n",
			want:   "alpha\nbravo\ncharlie\ndelta\n",
			wantOK: true,
		},
		{
			name:   "several appended lines per side",
			base:   "# changelog\n",
			sideA:  "# changelog\n- one\n- two\n",
			sideB:  "# changelog\n- three\n",
			want:   "# changelog\n- one\n- two\n- three\n",
			wantOK: true,
		},
		{
			name:   "an empty ancestor is a prefix of anything",
			base:   "",
			sideA:  "a\n",
			sideB:  "b\n",
			want:   "a\nb\n",
			wantOK: true,
		},
		{
			name:   "a side without a trailing newline still cannot fuse lines",
			base:   "x\n",
			sideA:  "x\nfrom a",
			sideB:  "x\nfrom b\n",
			want:   "x\nfrom a\nfrom b\n",
			wantOK: true,
		},
		{
			// The whole point of the rule: an edit to an existing line is two
			// people disagreeing about that line, which nothing here may settle.
			name:  "one side edited an existing line",
			base:  "alpha\nbravo\n",
			sideA: "alpha\nBRAVO\ncharlie\n",
			sideB: "alpha\nbravo\ndelta\n",
		},
		{
			name:  "one side deleted an existing line",
			base:  "alpha\nbravo\n",
			sideA: "alpha\ncharlie\n",
			sideB: "alpha\nbravo\ndelta\n",
		},
		{
			// Same line from both sides: keeping it once and keeping it twice
			// are both defensible, so it is not deterministic.
			name:  "both sides appended the same line",
			base:  "alpha\n",
			sideA: "alpha\nshared\n",
			sideB: "alpha\nshared\n",
		},
		{
			name:  "one side inserted in the middle rather than appending",
			base:  "alpha\ncharlie\n",
			sideA: "alpha\nbravo\ncharlie\n",
			sideB: "alpha\ncharlie\ndelta\n",
		},
		{
			name:  "the ancestor's last line has no terminator",
			base:  "alpha",
			sideA: "alphaX\n",
			sideB: "alphaY\n",
		},
		{
			name:  "only one side appended",
			base:  "alpha\n",
			sideA: "alpha\nbravo\n",
			sideB: "alpha\n",
		},
		{
			name:  "binary content is never resolved",
			base:  "a\x00b",
			sideA: "a\x00b c",
			sideB: "a\x00b d",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got, ok := appendOnlyResolution([]byte(tc.base), []byte(tc.sideA), []byte(tc.sideB))
			if ok != tc.wantOK {
				t.Fatalf("resolved=%v, want %v (got %q)", ok, tc.wantOK, got)
			}
			if ok && string(got) != tc.want {
				t.Fatalf("resolution = %q, want %q", got, tc.want)
			}
		})
	}
}

// The result must not depend on anything but the three inputs: the same
// conflict resolved twice has to produce the same bytes, or the "deterministic"
// in "deterministic low-risk" means nothing.
func TestAppendOnlyResolutionIsDeterministic(t *testing.T) {
	t.Parallel()
	base, a, b := []byte("head\n"), []byte("head\nfrom-a\n"), []byte("head\nfrom-b\n")
	first, ok := appendOnlyResolution(base, a, b)
	if !ok {
		t.Fatal("expected the append-only case to resolve")
	}
	for i := 0; i < 5; i++ {
		again, ok := appendOnlyResolution(base, a, b)
		if !ok || string(again) != string(first) {
			t.Fatalf("round %d produced %q, want %q", i, again, first)
		}
	}
}
