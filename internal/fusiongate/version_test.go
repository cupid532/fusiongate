package fusiongate

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// Version always carries exactly two decimal digits. Trailing zeros are significant:
// abbreviating V1.30 to V1.3 makes an increment from V1.29 read as a downgrade in the
// console sidebar, which is what the old formatting rule produced.
func TestVersionUsesTwoDecimalDigits(t *testing.T) {
	if !regexp.MustCompile(`^V\d+\.\d{2}$`).MatchString(Version) {
		t.Fatalf("Version = %q, want the form V<major>.<two digits>, for example V1.30", Version)
	}
}

// The hundredths encoding is what makes the increments well defined, so check that the
// declared version round-trips through it.
func TestVersionRoundTripsThroughIntegerHundredths(t *testing.T) {
	major, minor, ok := strings.Cut(strings.TrimPrefix(Version, "V"), ".")
	if !ok {
		t.Fatalf("Version = %q has no decimal part", Version)
	}
	majorValue, err := strconv.Atoi(major)
	if err != nil {
		t.Fatalf("Version = %q has a non-numeric major part", Version)
	}
	minorValue, err := strconv.Atoi(minor)
	if err != nil {
		t.Fatalf("Version = %q has a non-numeric hundredths part", Version)
	}
	hundredths := majorValue*100 + minorValue
	rendered := "V" + strconv.Itoa(hundredths/100) + "." + fixedTwoDigits(hundredths%100)
	if rendered != Version {
		t.Fatalf("re-rendering %d hundredths gives %q, want %q", hundredths, rendered, Version)
	}
}

func fixedTwoDigits(value int) string {
	text := strconv.Itoa(value)
	if len(text) == 1 {
		return "0" + text
	}
	return text
}

// Every release heading in the changelog follows the same rule, so the file reads
// monotonically instead of mixing V1.3 and V1.29.
func TestChangelogHeadingsUseTwoDecimalDigits(t *testing.T) {
	data, err := readRepoFile("CHANGELOG.md")
	if err != nil {
		t.Skipf("changelog not readable from the test working directory: %v", err)
	}
	heading := regexp.MustCompile(`(?m)^## (V\S+)`)
	twoDigits := regexp.MustCompile(`^V\d+\.\d{2}$`)
	found := 0
	for _, match := range heading.FindAllStringSubmatch(data, -1) {
		found++
		if !twoDigits.MatchString(match[1]) {
			t.Errorf("changelog heading %q does not use two decimal digits", match[1])
		}
	}
	if found == 0 {
		t.Fatal("no release headings found in the changelog")
	}
	if !strings.Contains(data, "## "+Version) {
		t.Errorf("the changelog has no entry for the current version %s", Version)
	}
}

// readRepoFile reads a file from the repository root, which sits two levels above the
// package directory during `go test`.
func readRepoFile(name string) (string, error) {
	data, err := os.ReadFile(filepath.Join("..", "..", name))
	if err != nil {
		return "", err
	}
	return string(data), nil
}
