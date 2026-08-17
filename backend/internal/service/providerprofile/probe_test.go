package providerprofile

import "testing"

func TestMergeEnv_OverrideWins(t *testing.T) {
	base := []string{"HOME=/real/host/home", "PATH=/usr/bin", "UNRELATED=x"}
	overrides := map[string]string{"HOME": "/isolated/user-a"}
	out := mergeEnv(base, overrides)

	seen := map[string]string{}
	for _, kv := range out {
		for i := 0; i < len(kv); i++ {
			if kv[i] == '=' {
				seen[kv[:i]] = kv[i+1:]
				break
			}
		}
	}
	if seen["HOME"] != "/isolated/user-a" {
		t.Fatalf("HOME override did not win: %+v", seen)
	}
	if seen["PATH"] != "/usr/bin" {
		t.Fatalf("unrelated base var was dropped: %+v", seen)
	}
	for _, kv := range out {
		if kv == "HOME=/real/host/home" {
			t.Fatal("real host HOME leaked into merged env alongside the override")
		}
	}
}

func TestAuthStateFromJSONLoggedIn(t *testing.T) {
	cases := []struct {
		name string
		out  string
		want string
	}{
		{"logged in", `{"loggedIn":true}`, "authenticated"},
		{"logged out", `{"loggedIn":false}`, "unauthenticated"},
		{"garbage", `not json`, "unknown"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := authStateFromJSONLoggedIn([]byte(c.out))
			if string(got) != c.want {
				t.Fatalf("got %s, want %s", got, c.want)
			}
		})
	}
}
