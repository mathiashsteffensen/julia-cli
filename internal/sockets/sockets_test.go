package sockets

import (
	"context"
	"net"
	"os"
	"runtime"
	"strings"
	"testing"
)

const table = `Active Internet connections (including servers)
Proto Recv-Q Send-Q  Local Address          Foreign Address        (state)          rxbytes      txbytes  rhiwat  shiwat          process:pid    state  options           gencnt    flags   flags1 usecnt rtncnt fltrs
tcp4       0      0  192.168.8.114.61822    34.86.145.125.443      SYN_SENT               0            0  131072  131072      quantrading:9399   00104 00000000 000000000631ca9b 00000080 04000900      2      0 000000
tcp46      0      0  *.8081                 *.*                    LISTEN                 0            0  131072  131072      quantrading:9386   00100 00000006 0000000005fd9930 00000000 00000800      1      0 000000
tcp4       0      0  127.0.0.1.5432         *.*                    LISTEN                 0            0  131072  131072         postgres:37135  00000 00000006 0000000005ebd387 00000000 00000800      1      0 000000
tcp6       0      0  ::1.5432               *.*                    LISTEN                 0            0  131072  131072         postgres:37135  00000 00000006 0000000005ebd386 00000000 00000800      1      0 000000
tcp4       0      0  127.0.0.1.41343        *.*                    LISTEN                 0            0  131072  131072        LM Studio:21297  00100 00000106 000000000330473f 00000001 00000800      1      0 000000
tcp4       0      0  127.0.0.1.52001        127.0.0.1.41343        ESTABLISHED         1200          300  131072  131072     Slack Helper:22352  00102 00000000 000000000631c3e3 00000000 04200900      1      0 000002
udp4       0      0  *.61679                *.*                                           0          482  786896    9216          syslogd:362    00180 00000000 00000000062a67d3 00000000 04000800      1      0 000002
udp4       0      0  192.168.8.114.57822    52.29.238.212.443                         16550         6821 1048576   29040     Slack Helper:22352  00102 00000000 000000000631c3e3 00000000 04200900      1      0 000002
`

func TestParse(t *testing.T) {
	socks, err := parse(table)
	if err != nil {
		t.Fatal(err)
	}
	want := []Socket{
		{"tcp4", Endpoint{"192.168.8.114", 61822}, Endpoint{"34.86.145.125", 443}, "SYN_SENT", 9399},
		{"tcp46", Endpoint{"*", 8081}, Endpoint{"*", 0}, "LISTEN", 9386},
		{"tcp4", Endpoint{"127.0.0.1", 5432}, Endpoint{"*", 0}, "LISTEN", 37135},
		{"tcp6", Endpoint{"::1", 5432}, Endpoint{"*", 0}, "LISTEN", 37135},
		{"tcp4", Endpoint{"127.0.0.1", 41343}, Endpoint{"*", 0}, "LISTEN", 21297},
		{"tcp4", Endpoint{"127.0.0.1", 52001}, Endpoint{"127.0.0.1", 41343}, "ESTABLISHED", 22352},
		{"udp4", Endpoint{"*", 61679}, Endpoint{"*", 0}, "", 362},
		{"udp4", Endpoint{"192.168.8.114", 57822}, Endpoint{"52.29.238.212", 443}, "", 22352},
	}
	if len(socks) != len(want) {
		t.Fatalf("got %d sockets: %+v", len(socks), socks)
	}
	for i := range want {
		if socks[i] != want[i] {
			t.Errorf("row %d = %+v, want %+v", i, socks[i], want[i])
		}
	}
	listening := map[int]bool{}
	for _, s := range socks {
		if s.Listening() && s.Local.Loopback() {
			listening[s.Local.Port] = true
		}
	}
	for _, port := range []int{8081, 5432, 41343, 61679} {
		if !listening[port] {
			t.Errorf("port %d is not listed as listening on loopback", port)
		}
	}
	if listening[52001] || listening[57822] {
		t.Error("a connected socket counts as listening")
	}
	for _, bad := range []string{"", "Proto Recv-Q Send-Q Local Address Foreign Address (state)\ntcp4 0 0 *.80 *.* LISTEN\n", strings.Replace(table, "postgres:37135", "postgres", 1)} {
		if _, err := parse(bad); err == nil {
			t.Errorf("parsed an unreadable table:\n%s", bad)
		}
	}
}

// The real table names this process as the owner of its own listener and of
// both ends of a connection to it.
func TestListFindsOwnSockets(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("sockets are listed on macOS only")
	}
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	conn, err := net.Dial("tcp", l.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	socks, err := List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	client := conn.LocalAddr().(*net.TCPAddr).Port
	var listener, dialer bool
	for _, s := range socks {
		switch {
		case s.Listening() && s.Local.Port == port:
			listener = s.PID == os.Getpid()
		case s.Local.Port == client && s.Remote.Port == port:
			dialer = s.PID == os.Getpid()
		}
	}
	if !listener || !dialer {
		t.Fatalf("own listener found %v, own connection found %v", listener, dialer)
	}
}
