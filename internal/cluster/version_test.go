package cluster

import (
	"regexp"
	"testing"
)

func TestVersionPattern(t *testing.T) {
	avail := []string{"26.3.39.7", "26.3.41.4", "26.3.41.40", "26.30.1.2", "26.3.41.4-1", "26.8.1.1", "25.3.9.1"}
	match := func(line string) (out []string) {
		re := regexp.MustCompile(versionPattern(line)) // grep -E semantics for this simple pattern
		for _, v := range avail {
			if re.MatchString(v) {
				out = append(out, v)
			}
		}
		return
	}
	if got := match("26.3"); len(got) != 4 { // 26.3.39.7, 26.3.41.4, 26.3.41.40, 26.3.41.4-1; not 26.30
		t.Fatalf("line 26.3 matched %v", got)
	}
	if got := match("26.3.41.4"); len(got) != 2 || got[0] != "26.3.41.4" || got[1] != "26.3.41.4-1" {
		t.Fatalf("exact 26.3.41.4 matched %v (want the deb and rpm forms only, not 26.3.41.40)", got)
	}
}
