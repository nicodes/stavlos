package sandbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"runtime"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

func init() {
	if len(os.Args) > 0 && os.Args[0] == helperArg0 {
		os.Exit(helper())
	}
}

// helper sets up the sandbox and executes the command. It returns only on
// failure, with the shell's "cannot execute" status.
func helper() int {
	// Landlock, no_new_privs and exec all act on the calling thread.
	runtime.LockOSThread()
	fail := func(format string, a ...any) int {
		fmt.Fprintf(os.Stderr, "stavlos sandbox: "+format+"\n", a...)
		return 126
	}
	var w wire
	if err := json.Unmarshal([]byte(os.Getenv(specEnv)), &w); err != nil {
		return fail("bad spec: %v", err)
	}
	args := os.Args[1:]
	if len(args) < 2 || args[0] != "--" {
		return fail("usage: %s -- program [args]", helperArg0)
	}
	args = args[1:]
	env := make([]string, 0, len(os.Environ()))
	for _, kv := range os.Environ() {
		if !strings.HasPrefix(kv, specEnv+"=") && !strings.HasPrefix(kv, stageEnv+"=") {
			env = append(env, kv)
		}
	}
	if w.Mounts && os.Getenv(stageEnv) == "" {
		return enterNamespaces()
	}
	writable := w.Writable
	if w.Mounts {
		if err := mountView(w.Spec); err != nil {
			return fail("%v", err)
		}
		// The command gets no capability: without an ambient set, executing
		// it as a non-root user drops them all.
		if err := unix.Prctl(unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0); err != nil {
			return fail("clear capabilities: %v", err)
		}
		hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
		var none [2]unix.CapUserData
		if err := unix.Capset(&hdr, &none[0]); err != nil {
			return fail("drop capabilities: %v", err)
		}
	}
	switch {
	case w.Tmp != "" && w.Mounts && w.PrivateTmp:
		writable = append(writable, "/tmp")
		env = setEnv(env, "TMPDIR", "/tmp")
	case w.Tmp != "":
		writable = append(writable, w.Tmp) // used where it is
		env = setEnv(env, "TMPDIR", w.Tmp)
	}
	if err := restrict(writable, w.Network); err != nil {
		return fail("%v", err)
	}
	if w.Probe {
		return 0
	}
	err := unix.Exec(args[0], args, env)
	return fail("exec %s: %v", args[0], err)
}

// stageEnv marks the copy of the helper that is already inside the user and
// mount namespaces.
const stageEnv = "STAVLOS_SANDBOX_STAGE"

// enterNamespaces runs this helper again inside a new user and mount
// namespace and stands in for it: same arguments, same files, the same
// process group (so what stops the command stops both), and its exit status.
// Were this copy to die first, the kernel kills the other.
func enterNamespaces() int {
	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "stavlos sandbox: %v\n", err)
		return 126
	}
	uid, gid := os.Getuid(), os.Getgid()
	cmd := exec.Command(self)
	cmd.Args = os.Args
	cmd.Env = append(os.Environ(), stageEnv+"=2")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags:                 syscall.CLONE_NEWUSER | syscall.CLONE_NEWNS,
		UidMappings:                []syscall.SysProcIDMap{{ContainerID: uid, HostID: uid, Size: 1}},
		GidMappings:                []syscall.SysProcIDMap{{ContainerID: gid, HostID: gid, Size: 1}},
		GidMappingsEnableSetgroups: false,
		// The helper keeps its user identity, so executing it would drop the
		// namespace's capabilities; an ambient CAP_SYS_ADMIN carries the one
		// it needs to mount, and the helper clears it before the command.
		AmbientCaps: []uintptr{unix.CAP_SYS_ADMIN},
		Pdeathsig:   syscall.SIGKILL,
	}
	// Signals meant for the command reach it through the process group; this
	// copy only has to outlive it to report how it ended. They are caught, not
	// ignored: an ignored signal stays ignored across exec, and the command
	// would inherit a SIGTERM it could never be stopped with.
	signal.Notify(make(chan os.Signal, 8), syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP, syscall.SIGQUIT)
	err = cmd.Run()
	var exit *exec.ExitError
	switch {
	case err == nil:
		return 0
	case errors.As(err, &exit):
		if ws, ok := exit.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
			return 128 + int(ws.Signal())
		}
		return exit.ExitCode()
	}
	fmt.Fprintf(os.Stderr, "stavlos sandbox: user namespaces unavailable: %v\n", err)
	return 126
}

// setEnv replaces or adds one variable.
func setEnv(env []string, name, value string) []string {
	out := env[:0:0]
	for _, kv := range env {
		if !strings.HasPrefix(kv, name+"=") {
			out = append(out, kv)
		}
	}
	return append(out, name+"="+value)
}

// mountView makes the helper's mount namespace private, then mounts the
// scratch directory over /tmp, binds control files read-only and hides
// paths. The order matters: /tmp's source may lie in a hidden directory.
func mountView(s Spec) error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("private mounts: %v", err)
	}
	if s.Tmp != "" && s.PrivateTmp {
		if err := unix.Mount(s.Tmp, "/tmp", "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
			return fmt.Errorf("mount %s on /tmp: %v", s.Tmp, err)
		}
	}
	for _, p := range s.ReadOnly {
		if _, err := os.Lstat(p); err != nil {
			continue
		}
		if err := bindReadOnly(p, p); err != nil {
			return err
		}
	}
	for _, p := range s.Hidden {
		if err := hide(p); err != nil {
			return err
		}
	}
	return nil
}

// hide covers a directory with an empty read-only tmpfs and a file with
// /dev/null.
func hide(p string) error {
	st, err := os.Lstat(p)
	if err != nil {
		return nil // nothing there to hide
	}
	if st.IsDir() {
		if err := unix.Mount("tmpfs", p, "tmpfs", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC|unix.MS_RDONLY, "size=4k,mode=0555"); err != nil {
			return fmt.Errorf("hide %s: %v", p, err)
		}
		return nil
	}
	if st.Mode()&os.ModeSymlink != 0 {
		return nil // a mount on a link lands on its target; hide the target by name instead
	}
	return bindReadOnly("/dev/null", p)
}

// bindReadOnly bind-mounts src on dst and remounts it read-only, keeping
// the flags of the mount it came from (a user namespace may not clear them).
func bindReadOnly(src, dst string) error {
	if err := unix.Mount(src, dst, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind %s: %v", dst, err)
	}
	var sf unix.Statfs_t
	if err := unix.Statfs(dst, &sf); err != nil {
		return fmt.Errorf("statfs %s: %v", dst, err)
	}
	keep := uintptr(sf.Flags) & (unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC | unix.MS_NOATIME | unix.MS_NODIRATIME | unix.MS_RELATIME)
	if err := unix.Mount("", dst, "", unix.MS_BIND|unix.MS_REMOUNT|unix.MS_RDONLY|keep, ""); err != nil {
		return fmt.Errorf("read-only %s: %v", dst, err)
	}
	return nil
}

// Landlock rights (include/uapi/linux/landlock.h).
const (
	fsWriteFile  = unix.LANDLOCK_ACCESS_FS_WRITE_FILE
	fsTruncate   = unix.LANDLOCK_ACCESS_FS_TRUNCATE
	fsRefer      = unix.LANDLOCK_ACCESS_FS_REFER
	fsDirChanges = unix.LANDLOCK_ACCESS_FS_REMOVE_DIR | unix.LANDLOCK_ACCESS_FS_REMOVE_FILE | unix.LANDLOCK_ACCESS_FS_MAKE_CHAR |
		unix.LANDLOCK_ACCESS_FS_MAKE_DIR | unix.LANDLOCK_ACCESS_FS_MAKE_REG | unix.LANDLOCK_ACCESS_FS_MAKE_SOCK |
		unix.LANDLOCK_ACCESS_FS_MAKE_FIFO | unix.LANDLOCK_ACCESS_FS_MAKE_BLOCK | unix.LANDLOCK_ACCESS_FS_MAKE_SYM
	netTCP  = unix.LANDLOCK_ACCESS_NET_BIND_TCP | unix.LANDLOCK_ACCESS_NET_CONNECT_TCP
	scoped  = unix.LANDLOCK_SCOPE_ABSTRACT_UNIX_SOCKET | unix.LANDLOCK_SCOPE_SIGNAL
	ruleDir = 1 // LANDLOCK_RULE_PATH_BENEATH

	createRulesetVersion = 1 // LANDLOCK_CREATE_RULESET_VERSION
)

// alwaysWritable are the device files and directories every program
// expects to write.
var alwaysWritable = []string{"/dev/null", "/dev/zero", "/dev/full", "/dev/tty", "/dev/ptmx", "/dev/pts", "/dev/shm"}

type rulesetAttr struct{ fs, net, scoped uint64 }

type pathBeneathAttr struct {
	access uint64
	fd     int32
	_      [4]byte
}

// abi is the Landlock ABI version the kernel offers, 0 or less without it.
func abi() int {
	v, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, 0, 0, createRulesetVersion)
	if errno != 0 {
		return 0
	}
	return int(v)
}

// restrict confines the calling thread, and what it executes, with
// Landlock: writes only beneath writable, TCP only when network is on, and
// no signals or abstract Unix sockets across the sandbox's edge. Rights the
// kernel does not know are left out rather than failing.
func restrict(writable []string, network bool) error {
	v := abi()
	if v <= 0 {
		return fmt.Errorf("landlock unavailable")
	}
	fileRights := uint64(fsWriteFile)
	dirRights := uint64(fsWriteFile | fsDirChanges)
	if v >= 2 {
		dirRights |= fsRefer
	}
	if v >= 3 {
		fileRights |= fsTruncate
		dirRights |= fsTruncate
	}
	attr := rulesetAttr{fs: dirRights}
	if v >= 4 && !network {
		attr.net = netTCP
	}
	if v >= 6 {
		attr.scoped = scoped
	}
	fd, _, errno := unix.Syscall(unix.SYS_LANDLOCK_CREATE_RULESET, uintptr(unsafe.Pointer(&attr)), unsafe.Sizeof(attr), 0)
	if errno != 0 {
		return fmt.Errorf("landlock ruleset: %v", errno)
	}
	defer unix.Close(int(fd))
	for _, p := range append(append([]string(nil), alwaysWritable...), writable...) {
		pf, err := unix.Open(p, unix.O_PATH|unix.O_CLOEXEC, 0)
		if err != nil {
			continue // not there: nothing to allow
		}
		var st unix.Stat_t
		rights := fileRights
		if unix.Fstat(pf, &st) == nil && st.Mode&unix.S_IFMT == unix.S_IFDIR {
			rights = dirRights
		}
		rule := pathBeneathAttr{access: rights, fd: int32(pf)}
		_, _, errno := unix.Syscall6(unix.SYS_LANDLOCK_ADD_RULE, fd, ruleDir, uintptr(unsafe.Pointer(&rule)), 0, 0, 0)
		unix.Close(pf)
		if errno != 0 {
			return fmt.Errorf("landlock rule for %s: %v", p, errno)
		}
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %v", err)
	}
	if _, _, errno := unix.Syscall(unix.SYS_LANDLOCK_RESTRICT_SELF, fd, 0, 0); errno != 0 {
		return fmt.Errorf("landlock restrict: %v", errno)
	}
	return nil
}
