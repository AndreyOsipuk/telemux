package stack

import "testing"

func TestAssess(t *testing.T) {
	cases := []struct {
		name, cur, lat  string
		custom, updates bool
	}{
		{"тот же релиз", "3.5.17", "3.5.17", false, false},
		{"новее патч", "3.5.16", "3.5.17", false, true},
		{"ведущая v", "v1.14.0", "v1.15.0", false, true},
		{"своя сборка на том же ядре - не обновление", "1.15.0-webalias", "v1.15.0", true, false},
		{"своя сборка на старом ядре", "1.15.0-webalias", "v1.16.0", true, true},
		{"git describe - своя сборка", "v1.12.0-15-g039ef04", "v1.15.0", true, true},
		{"на машине новее релиза", "3.5.18", "3.5.17", false, false},
		{"неразборчивая версия", "mtg-multi 2510861", "v1.15.0", true, false},
		{"версии нет", "", "3.5.17", false, false},
		{"релиз не разобран", "3.5.16", "nightly", false, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			a := Assess(c.cur, c.lat)
			if a.CustomBuild != c.custom || a.UpdateAvailable != c.updates {
				t.Fatalf("Assess(%q, %q) = %+v", c.cur, c.lat, a)
			}
		})
	}
}

func TestSameVersion(t *testing.T) {
	if !SameVersion("1.15.0", "v1.15.0") || SameVersion("1.15.0-webalias", "v1.15.0") || SameVersion("3.5.16", "3.5.17") {
		t.Fatal("SameVersion")
	}
}

func TestParse(t *testing.T) {
	sha := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	c, err := Parse([]byte(`{"telemt":{"version":"3.5.17","sha256":"` + sha + `","size":10}}`))
	if err != nil || c[ProductTelemt].Size != 10 {
		t.Fatalf("%v %v", c, err)
	}
	for _, bad := range []string{
		`{"xray":{"version":"1","sha256":"` + sha + `","size":1}}`,
		`{"mtg":{"version":"1","sha256":"ABC","size":1}}`,
		`{"mtg":{"version":"","sha256":"` + sha + `","size":1}}`,
		`{"mtg":{"version":"1","sha256":"` + sha + `","size":0}}`,
		`[]`,
	} {
		if _, err := Parse([]byte(bad)); err == nil {
			t.Fatalf("принят %s", bad)
		}
	}
	b, _ := c.Marshal()
	if _, err := Parse(b); err != nil {
		t.Fatal(err)
	}
}
