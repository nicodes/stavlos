package peercred

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

func TestOfAndDescendsFrom(t *testing.T) {
	sock := filepath.Join(t.TempDir(), "s.sock")
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		if c, err := ln.Accept(); err == nil {
			defer c.Close()
			buf := make([]byte, 1)
			_, _ = c.Read(buf)
		}
	}()
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	cred, err := OfSelf(c.(*net.UnixConn))
	if err != nil || cred.PID != os.Getpid() || cred.UID != os.Getuid() {
		t.Fatalf("cred %+v err %v", cred, err)
	}

	child := exec.Command("sh", "-c", "sleep 5")
	if err := child.Start(); err != nil {
		t.Fatal(err)
	}
	defer child.Process.Kill()
	if !DescendsFrom(child.Process.Pid, os.Getpid()) {
		t.Fatal("a child should descend from its parent")
	}
	if DescendsFrom(os.Getpid(), child.Process.Pid) || DescendsFrom(os.Getpid(), os.Getpid()) {
		t.Fatal("descent runs one way and a process is not its own descendant")
	}
	if p, err := ParentOf(os.Getpid()); err != nil || p != os.Getppid() {
		t.Fatalf("parent %d err %v", p, err)
	}
}
