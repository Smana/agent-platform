// SPDX-License-Identifier: Apache-2.0

package semverdiff

import "testing"

const digA = "sha256:" + "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
const digB = "sha256:" + "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"

func TestShapes(t *testing.T) {
	f := "infrastructure/base/x/helmrelease.yaml"
	for name, c := range map[string]struct {
		base, head   string
		patch, minor bool
	}{
		"a chart patch (#2537)":        {"      version: 3.9.1\n", "      version: 3.9.2\n", true, true},
		"a 0.x patch (#2532)":          {"    tag: v0.75.0\n", "    tag: v0.75.1\n", true, true},
		"a tag and its digest":         {"  tag: \"v1.2.3@" + digA + "\"\n", "  tag: \"v1.2.4@" + digB + "\"\n", true, true},
		"a digest alone":               {"  image: x@" + digA + "\n", "  image: x@" + digB + "\n", false, false},
		"a minor (#2471)":              {"      version: 2.12.4\n", "      version: 2.13.0\n", false, true},
		"a major":                      {"      version: 2.9.9\n", "      version: 3.0.0\n", false, false},
		"a downgrade":                  {"      version: 1.2.4\n", "      version: 1.2.3\n", false, false},
		"a pre-release":                {"      version: 1.2.3\n", "      version: 1.2.4-rc.1\n", false, false},
		"text beside the version":      {"      version: 1.2.3 # pinned\n", "      version: 1.2.4 # unpinned\n", false, false},
		"a line added":                 {"a: 1.2.3\n", "a: 1.2.4\nb: 1\n", false, false},
		"a line with no version moved": {"a: 1.2.3\nkey: x\n", "a: 1.2.4\nkey: y\n", false, false},
		"a lockfile integrity line":    {"\"integrity\": \"sha512-AAA\"\n", "\"integrity\": \"sha512-BBB\"\n", false, false},
		"two versions on one line":     {"img: a:1.2.3 b:4.5.6\n", "img: a:1.2.4 b:4.5.7\n", true, true},
		"one of two moves backwards":   {"img: a:1.2.3 b:4.5.6\n", "img: a:1.2.4 b:4.5.5\n", false, false},
		"an added file":                {"", "a: 1.2.3\n", false, false},

		"build metadata":                          {"version: 1.2.3\n", "version: 1.2.4+build.5\n", false, false},
		"build metadata on the base":              {"version: 1.2.3+b\n", "version: 1.2.4\n", false, false},
		"a v prefix dropped":                      {"tag: v1.2.3\n", "tag: 1.2.4\n", false, false},
		"a v prefix added":                        {"tag: 1.2.3\n", "tag: v1.2.4\n", false, false},
		"a fourth component":                      {"version: 1.2.3.4\n", "version: 1.2.4.4\n", false, false},
		"a tag bump, the digest kept":             {"tag: v1.2.3@" + digA + "\n", "tag: v1.2.4@" + digA + "\n", true, true},
		"a tag bump, the digest dropped":          {"tag: v1.2.3@" + digA + "\n", "tag: v1.2.4\n", false, false},
		"a minor disguised in a patch":            {"version: 1.2.9\n", "version: 1.3.10\n", false, true},
		"a minor with a lower patch":              {"version: 1.2.9\n", "version: 1.3.0\n", false, true},
		"a minor going back":                      {"version: 1.3.0\n", "version: 1.2.9\n", false, false},
		"a major then minor reset":                {"version: 1.9.9\n", "version: 2.10.0\n", false, false},
		"a whitespace-only line":                  {"version: 1.2.3\nkey:  x\n", "version: 1.2.4\nkey: x\n", false, false},
		"whitespace beside the version":           {"version: 1.2.3\n", "version:  1.2.4\n", false, false},
		"only whitespace changed":                 {"a:  1.2.3\n", "a: 1.2.3\n", false, false},
		"nothing changed":                         {"a: 1.2.3\n", "a: 1.2.3\n", false, false},
		"a huge number":                           {"v: 1.2.3\n", "v: 1.2.99999999999999999999\n", false, false},
		"a version replaced by text":              {"v: 1.2.3\n", "v: latest\n", false, false},
		"a version appearing":                     {"v: latest\n", "v: 1.2.3\n", false, false},
		"a patch beside an untouched pin":         {"a: 1.2.3\nb: 9.9.9\n", "a: 1.2.4\nb: 9.9.9\n", true, true},
		"a minor on one line, a patch on another": {"a: 1.2.3\nb: 4.5.6\n", "a: 1.3.0\nb: 4.5.7\n", false, true},
		"a trailing newline dropped":              {"a: 1.2.3\n", "a: 1.2.4", true, true},
	} {
		t.Run(name, func(t *testing.T) {
			base, head := map[string]string{f: c.base}, map[string]string{f: c.head}
			if ok, why := Patch(base, head); ok != c.patch {
				t.Fatalf("Patch %v (%s), want %v", ok, why, c.patch)
			}
			if ok, why := Minor(base, head); ok != c.minor {
				t.Fatalf("Minor %v (%s), want %v", ok, why, c.minor)
			}
		})
	}
	if ok, _ := Patch(map[string]string{}, map[string]string{}); ok {
		t.Fatal("an empty diff is no bump")
	}
	if ok, _ := Patch(map[string]string{"a": "v: 1.2.3\n"}, map[string]string{"b": "v: 1.2.4\n"}); ok {
		t.Fatal("a renamed file is no bump")
	}
	if ok, _ := Patch(map[string]string{"a": "v: 1.2.3\n", "c": "x\n"}, map[string]string{"a": "v: 1.2.4\n"}); ok {
		t.Fatal("a deleted file is no bump")
	}
}

func FuzzNeverAcceptsWhatMasksDiffer(f *testing.F) {
	f.Add("a: 1.2.3\n", "a: 1.2.4\n")
	f.Add("v0.1.0@"+digA, "v0.1.1@"+digB)
	f.Fuzz(func(t *testing.T, a, b string) {
		ok, _ := Patch(map[string]string{"f": a}, map[string]string{"f": b})
		if ok && (a == b || len(lines(a)) != len(lines(b))) {
			t.Fatalf("accepted %q -> %q", a, b)
		}
		if okM, _ := Minor(map[string]string{"f": a}, map[string]string{"f": b}); ok && !okM {
			t.Fatalf("a patch is a minor: %q -> %q", a, b)
		}
	})
}
