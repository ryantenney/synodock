package dsm

import "testing"

func TestValidateProfile(t *testing.T) {
	for _, tc := range []struct {
		name    string
		profile string
		ok      bool
	}{
		{"good", `{"name":"web","image":"nginx:1"}`, true},
		{"no name", `{"image":"nginx:1"}`, false},
		{"no image", `{"name":"web"}`, false},
		{"not json", `name: web`, false},
	} {
		err := ValidateProfile([]byte(tc.profile))
		if (err == nil) != tc.ok {
			t.Errorf("%s: err %v, want ok=%v", tc.name, err, tc.ok)
		}
	}
}
