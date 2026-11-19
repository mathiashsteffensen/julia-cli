package browser

import "testing"

func TestClientPID(t *testing.T) {
	const client = "127.0.0.1:58980"
	const proxy = "127.0.0.1:58979"
	const serverSocket = "p123\nf11\nn127.0.0.1:58979->127.0.0.1:58980\n"
	const clientSocket = "p456\nf10\nn127.0.0.1:58980->127.0.0.1:58979\n"

	if pid := clientPID(serverSocket+clientSocket, client, proxy); pid != 456 {
		t.Errorf("client socket owner = %d, want 456", pid)
	}

	if pid := clientPID(serverSocket, client, proxy); pid != 0 {
		t.Errorf("only the proxy socket is visible, got client PID %d", pid)
	}
}
