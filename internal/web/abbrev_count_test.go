package web

import "testing"

func TestAbbrevCount(t *testing.T) {
	cases := []struct {
		in   int
		want string
	}{
		{0, "0"},
		{999, "999"},
		{1000, "1.0k"},
		{1049, "1.0k"},
		{1050, "1.1k"},
		{2786, "2.8k"},
		{9949, "9.9k"},
		{9950, "10k"},
		{12345, "12k"},
		{99999, "100k"},
		{999499, "999k"},
		{999500, "1.0M"},
		{1048576, "1.0M"},
	}
	for _, c := range cases {
		got := abbrevCount(c.in)
		if got != c.want {
			t.Errorf("abbrevCount(%d) = %q, want %q", c.in, got, c.want)
		}
		if len(got) > 4 {
			t.Errorf("abbrevCount(%d) = %q, wider than the 4ch gutter", c.in, got)
		}
	}

	for n := 0; n <= 2_000_000; n++ {
		if got := abbrevCount(n); len(got) > 4 {
			t.Fatalf("abbrevCount(%d) = %q, wider than the 4ch gutter", n, got)
		}
	}
}
