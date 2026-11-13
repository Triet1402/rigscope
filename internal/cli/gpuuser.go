package cli

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/Triet1402/rigscope/internal/config"
	"github.com/Triet1402/rigscope/internal/platform/fsutil"
	"github.com/Triet1402/rigscope/internal/platform/systemd"
)

// User-scope NVIDIA BAR0 register access.
//
// A user service gets its supplementary groups once, when the systemd user
// manager starts. Adding the user to a new group is therefore not enough until
// the whole session logs out, and a user unit cannot set SupplementaryGroups.
// The installer works around both limits:
//
//   - when the user has a private primary group (name equals the user, no other
//     members, no other account has that GID as primary), the rule grants that
//     group and access works as soon as udev applies it;
//   - otherwise it falls back to the shared rigscope-gpu group used by 0.2 and
//     by system scope, and the user must log out fully once.
//
// Only root-owned steps run through sudo, as plain commands with the caller's
// terminal attached for the password prompt. The rigscope binary is never
// re-executed under sudo: in user scope it is user-writable.

const userGPUGroup = systemd.GPUGroup

// UserAccount is one passwd record.
type UserAccount struct {
	Name string
	UID  int
	GID  int
}

// GroupRecord is one group record. Members are the supplementary members the
// group lists; a primary membership is recorded only on the passwd record.
type GroupRecord struct {
	Name    string
	GID     int
	Members []string
}

// AccountDatabase is the injectable local passwd/group database. Tests supply
// fixed records so no host account and no real /etc is read.
type AccountDatabase interface {
	// Current returns the invoking user's passwd record.
	Current() (UserAccount, error)
	// Passwd returns every passwd record.
	Passwd() ([]UserAccount, error)
	// Groups returns every group record.
	Groups() ([]GroupRecord, error)
}

// hostAccounts reads the account database from the host's /etc files below an
// optional test/image root.
type hostAccounts struct {
	Root string
	Euid int
}

func (h hostAccounts) Current() (UserAccount, error) {
	records, err := h.Passwd()
	if err != nil {
		return UserAccount{}, err
	}
	for _, record := range records {
		if record.UID == h.Euid {
			return record, nil
		}
	}
	return UserAccount{}, fmt.Errorf("uid %d is not listed in %s", h.Euid, filepath.Join(h.hostDir(), "etc", "passwd"))
}

func (h hostAccounts) Passwd() ([]UserAccount, error) {
	path := filepath.Join(h.hostDir(), "etc", "passwd")
	return parsePasswdFile(path)
}

func (h hostAccounts) Groups() ([]GroupRecord, error) {
	path := filepath.Join(h.hostDir(), "etc", "group")
	return parseGroupFile(path)
}

func (h hostAccounts) hostDir() string {
	if h.Root == "" {
		return string(filepath.Separator)
	}
	return h.Root
}

// envAccountName names the invoking account from the environment when the
// local passwd database cannot. Release builds are built with CGO_ENABLED=0,
// so os/user resolves only local files: an account that NSS serves but
// /etc/passwd does not list would otherwise leave the account unknown. USER and
// LOGNAME normally name the caller; a value is accepted only when it passes the
// same validation as a passwd name, because it ends up in a udev rule and in
// the usermod argv. Such an account is never treated as having a private group.
func envAccountName(getenv func(string) string) string {
	if getenv == nil {
		return ""
	}
	for _, key := range []string{"USER", "LOGNAME"} {
		name := strings.TrimSpace(getenv(key))
		if validAccountName(name) {
			return name
		}
	}
	return ""
}

// nsswitchAllowsPrivateGroup reports whether passwd and group are served only
// by local-file databases. With LDAP, SSSD or another NSS source involved, the
// /etc files under Root do not describe the real accounts, so an apparent
// private primary group cannot be trusted and the shared group is used.
func nsswitchAllowsPrivateGroup(root string) bool {
	base := string(filepath.Separator)
	if root != "" {
		base = root
	}
	file, err := os.Open(filepath.Join(base, "etc", "nsswitch.conf"))
	if err != nil {
		// No nsswitch.conf means the glibc defaults, which are local files.
		return true
	}
	defer file.Close()
	allowed := map[string]bool{
		"files":    true,
		"systemd":  true,
		"compat":   true,
		"altfiles": true,
	}
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if comment := strings.IndexByte(line, '#'); comment >= 0 {
			line = strings.TrimSpace(line[:comment])
		}
		fields := strings.Fields(line)
		if len(fields) < 2 || (fields[0] != "passwd:" && fields[0] != "group:") {
			continue
		}
		for _, source := range fields[1:] {
			if strings.HasPrefix(source, "[") {
				// Action items such as [NOTFOUND=return] follow a source.
				continue
			}
			if !allowed[source] {
				return false
			}
		}
	}
	return true
}

// groupGID resolves a group name through the account database.
func groupGID(db AccountDatabase, name string) (int, bool) {
	groups, err := db.Groups()
	if err != nil {
		return 0, false
	}
	for _, group := range groups {
		if group.Name == name {
			return group.GID, true
		}
	}
	return 0, false
}

// scanUserManagerGroups finds the invoking user's systemd --user manager by
// scanning /proc for a process named systemd with the same real uid, then
// returns the GIDs on its Groups: line. A manager whose cgroup names
// user@<uid>.service is preferred, because other systemd processes of the same
// uid can exist (for example a transient session scope) and need not carry the
// login session's groups. The first matching process is the fallback when no
// cgroup names the user manager. PID 1 is the system manager and is excluded.
// Not finding a manager is reported as an error so the caller can say the
// collector's session could not be checked.
func scanUserManagerGroups(procRoot string, uid int) ([]int, error) {
	entries, err := os.ReadDir(procRoot)
	if err != nil {
		return nil, err
	}
	var (
		fallback     []int
		haveFallback bool
	)
	for _, entry := range entries {
		if !entry.IsDir() || entry.Name() == "1" {
			continue
		}
		pid := entry.Name()
		if _, err := strconv.Atoi(pid); err != nil {
			continue
		}
		data, err := os.ReadFile(filepath.Join(procRoot, pid, "status"))
		if err != nil {
			continue
		}
		name, realUID, groups, ok := parseStatusGroups(string(data))
		if !ok || name != "systemd" || realUID != uid {
			continue
		}
		if userManagerCgroup(procRoot, pid, uid) {
			return groups, nil
		}
		if !haveFallback {
			fallback, haveFallback = groups, true
		}
	}
	if haveFallback {
		return fallback, nil
	}
	return nil, errors.New("no systemd --user manager found")
}

// userManagerCgroup reports whether /proc/<pid>/cgroup names the user manager
// unit for uid.
func userManagerCgroup(procRoot, pid string, uid int) bool {
	data, err := os.ReadFile(filepath.Join(procRoot, pid, "cgroup"))
	if err != nil {
		return false
	}
	return strings.Contains(string(data), fmt.Sprintf("user@%d.service", uid))
}

// parseStatusGroups extracts Name, the real uid and the numeric Groups from
// /proc/<pid>/status.
func parseStatusGroups(status string) (string, int, []int, bool) {
	var (
		name       string
		realUID    int
		groups     []int
		haveName   bool
		haveUID    bool
		haveGroups bool
	)
	for _, line := range strings.Split(status, "\n") {
		key, value, found := strings.Cut(line, ":")
		if !found {
			continue
		}
		switch key {
		case "Name":
			name = strings.TrimSpace(value)
			haveName = true
		case "Uid":
			fields := strings.Fields(value)
			if len(fields) == 0 {
				return "", 0, nil, false
			}
			parsed, err := strconv.Atoi(fields[0])
			if err != nil {
				return "", 0, nil, false
			}
			realUID = parsed
			haveUID = true
		case "Groups":
			for _, field := range strings.Fields(value) {
				parsed, err := strconv.Atoi(field)
				if err != nil {
					continue
				}
				groups = append(groups, parsed)
			}
			haveGroups = true
		}
	}
	return name, realUID, groups, haveName && haveUID && haveGroups
}

func parsePasswdFile(path string) ([]UserAccount, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var records []UserAccount
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 4 {
			continue
		}
		uid, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}
		gid, err := strconv.Atoi(fields[3])
		if err != nil {
			continue
		}
		records = append(records, UserAccount{Name: fields[0], UID: uid, GID: gid})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

func parseGroupFile(path string) ([]GroupRecord, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var records []GroupRecord
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ":")
		if len(fields) < 3 {
			continue
		}
		gid, err := strconv.Atoi(fields[2])
		if err != nil {
			continue
		}
		var members []string
		if len(fields) > 3 && strings.TrimSpace(fields[3]) != "" {
			for _, member := range strings.Split(fields[3], ",") {
				member = strings.TrimSpace(member)
				if member != "" {
					members = append(members, member)
				}
			}
		}
		records = append(records, GroupRecord{Name: fields[0], GID: gid, Members: members})
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	return records, nil
}

// privatePrimaryGroup decides whether the invoking user's primary group can be
// granted directly. The group must be private to the account: its name equals
// the user name, it lists no other member, and no other passwd entry uses that
// GID as its primary group. A shared GID would hand the rule to other accounts.
func privatePrimaryGroup(db AccountDatabase) (string, bool) {
	me, err := db.Current()
	if err != nil {
		return "", false
	}
	passwd, err := db.Passwd()
	if err != nil {
		return "", false
	}
	groups, err := db.Groups()
	if err != nil {
		return "", false
	}
	if !validAccountName(me.Name) {
		return "", false
	}
	var primary *GroupRecord
	for i := range groups {
		if groups[i].GID == me.GID {
			primary = &groups[i]
			break
		}
	}
	if primary == nil || primary.Name != me.Name {
		return "", false
	}
	for _, member := range primary.Members {
		if member != me.Name {
			return "", false
		}
	}
	for _, account := range passwd {
		if account.GID == me.GID && account.Name != me.Name {
			return "", false
		}
	}
	return me.Name, true
}

// groupExists reports whether a group is recorded. known is false when the
// group database could not be read; the caller then avoids a groupadd that
// might fail on an existing group.
func groupExists(db AccountDatabase, name string) (exists, known bool) {
	groups, err := db.Groups()
	if err != nil {
		return false, false
	}
	for _, group := range groups {
		if group.Name == name {
			return true, true
		}
	}
	return false, true
}

// validAccountName keeps a name from breaking the udev rule text or argv.
func validAccountName(name string) bool {
	if name == "" || name == "-" || strings.HasPrefix(name, "-") {
		return false
	}
	for _, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.':
		default:
			return false
		}
	}
	return true
}

// managedGPURuleExists reports whether the root-owned GPU rule exists and
// carries the Rigscope marker. The manifest keeps tracking such a file even
// when a later install does not pass --gpu-registers.
func managedGPURuleExists(paths config.Paths) bool {
	content, err := os.ReadFile(paths.UdevGPUTempsFile)
	return err == nil && systemd.IsManaged(string(content))
}

var ruleGroupPattern = regexp.MustCompile(`chgrp\s+(\S+)\s+/sys%p/resource0`)

// ruleGroup extracts the group a rendered GPU rule grants.
func ruleGroup(content string) string {
	match := ruleGroupPattern.FindStringSubmatch(content)
	if match == nil {
		return ""
	}
	return match[1]
}

// nvidiaDisplayDirs lists the sysfs directories of NVIDIA display functions
// (vendor 0x10de, base class 0x03) under the PCI bus tree.
func (d Deps) nvidiaDisplayDirs() ([]string, error) {
	root := d.hostPath("sys", "bus", "pci", "devices")
	entries, err := os.ReadDir(root)
	if err != nil {
		return nil, err
	}
	var dirs []string
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		dir := filepath.Join(root, entry.Name())
		vendor, err := sysfsAttribute(dir, "vendor")
		if err != nil || vendor != "0x10de" {
			continue
		}
		class, err := sysfsAttribute(dir, "class")
		if err != nil || !strings.HasPrefix(class, "0x03") {
			continue
		}
		dirs = append(dirs, dir)
	}
	sort.Strings(dirs)
	return dirs, nil
}

func sysfsAttribute(dir, name string) (string, error) {
	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(data)), nil
}

// gpuProbe is the result of trying to map BAR0 as the invoking user.
type gpuProbe struct {
	// devices is the number of NVIDIA display functions sysfs lists.
	devices int
	// readable is the number whose resource0 this session can open and map.
	readable int
	// suspended is the number of display functions whose runtime power state
	// is "suspended". They are present but deliberately not probed, because
	// any access, even an mmap, can wake a dGPU.
	suspended int
	// missing is the number whose resource0 does not exist.
	missing int
	// denied is set when at least one probe failed with EACCES/EPERM.
	denied bool
	// err is the first probe failure that was not a permission denial.
	err error
}

// probeGPUDisplays maps resource0 of every NVIDIA display function read-only
// and unmaps it, without reading a register word. It runs as the invoking user,
// so it proves what this session may map. A device whose runtime power state is
// suspended is skipped and counted separately: probing it could wake it. The
// probe does not prove the systemd --user manager carries the group: a fresh
// ssh or tty session can have group membership the long-running manager never
// received, so the shared-group case checks the manager separately.
func probeGPUDisplays(deps Deps) gpuProbe {
	dirs, err := deps.nvidiaDisplayDirs()
	if err != nil {
		return gpuProbe{err: err}
	}
	probe := gpuProbe{devices: len(dirs)}
	for _, dir := range dirs {
		if status, err := sysfsAttribute(dir, "power/runtime_status"); err == nil && status == "suspended" {
			probe.suspended++
			continue
		}
		path := filepath.Join(dir, "resource0")
		if err := deps.probeRegisterAccess(path); err != nil {
			switch {
			case errors.Is(err, fs.ErrPermission):
				probe.denied = true
			case errors.Is(err, fs.ErrNotExist):
				probe.missing++
			default:
				if probe.err == nil {
					probe.err = err
				}
			}
			continue
		}
		probe.readable++
	}
	return probe
}

// userGPUAccess describes how a user install will get register access.
type userGPUAccess struct {
	// group is the group the rule grants.
	group string
	// private is true when group is the user's private primary group.
	private bool
	// privateCandidate is the user's private primary group when the account
	// database describes one, even when NSS makes it unusable. It lets the
	// installer recognise and rewrite its own earlier rule.
	privateCandidate string
	// user is the invoking account name and groupMissing reports whether the
	// shared group must be created first.
	user         string
	groupMissing bool
	// member is true when the account already belongs to the shared group,
	// either as a listed supplementary member or through its primary GID.
	member bool
}

func planUserGPUAccess(deps Deps) userGPUAccess {
	access := userGPUAccess{group: userGPUGroup}
	db := deps.accounts()
	me, err := db.Current()
	if err == nil {
		access.user = me.Name
	} else {
		// The uid may be served by NSS only. Cgo-free release builds read just
		// the local files, so fall back to the name the environment carries.
		// That path never sets private below: an environment name cannot
		// prove a private group, so the shared group is used.
		access.user = envAccountName(deps.env)
	}
	if err == nil {
		if candidate, ok := privatePrimaryGroup(db); ok {
			access.privateCandidate = candidate
			// NSS-backed accounts and a uid missing from /etc/passwd cannot be
			// safely checked against the local group file, so use the shared
			// group, which works regardless of NSS.
			if nsswitchAllowsPrivateGroup(deps.Root) && me.Name == candidate {
				access.group = candidate
				access.private = true
			}
		}
	}
	if !access.private {
		exists, known := groupExists(db, userGPUGroup)
		access.groupMissing = known && !exists
		access.member = sharedGroupMember(db, access.user)
	}
	return access
}

// sharedGroupMember reports whether name already belongs to the shared group,
// either as a listed member or through its primary GID. An unreadable group
// database reports false, so the caller runs usermod, which is idempotent,
// rather than assume membership.
func sharedGroupMember(db AccountDatabase, name string) bool {
	if name == "" {
		return false
	}
	groups, err := db.Groups()
	if err != nil {
		return false
	}
	var shared *GroupRecord
	for i := range groups {
		if groups[i].Name == userGPUGroup {
			shared = &groups[i]
			break
		}
	}
	if shared == nil {
		return false
	}
	for _, member := range shared.Members {
		if member == name {
			return true
		}
	}
	me, err := db.Current()
	if err != nil {
		return false
	}
	return me.Name == name && me.GID == shared.GID
}

// useSharedGroup switches an access plan to the shared fallback. The shared
// group is what a system install or 0.2 already granted, so an existing rule
// for it is adopted rather than replaced.
func useSharedGroup(deps Deps, access *userGPUAccess) {
	access.group = userGPUGroup
	access.private = false
	exists, known := groupExists(deps.accounts(), userGPUGroup)
	access.groupMissing = known && !exists
	access.member = sharedGroupMember(deps.accounts(), access.user)
}

// userGPUInstallCommands lists the root-owned steps for a user installation.
// The first two only exist for the shared-group fallback.
func userGPUInstallCommands(access userGPUAccess, tempPath, rulePath string) [][]string {
	var commands [][]string
	commands = append(commands, userGPUMembershipCommands(access)...)
	commands = append(commands,
		[]string{"install", "-m", "0644", "-o", "root", "-g", "root", tempPath, rulePath},
		[]string{"udevadm", "control", "--reload"},
		[]string{"udevadm", "trigger", "--subsystem-match=pci", "--attr-match=vendor=0x10de"},
		[]string{"udevadm", "settle"},
	)
	return commands
}

// userGPUMembershipCommands lists only the group steps of the fallback.
// groupadd runs only when the group is missing, and usermod only when the
// account is not already a member: a reinstall on a host where the user is
// already in the shared group must not run any of these.
func userGPUMembershipCommands(access userGPUAccess) [][]string {
	if access.private {
		return nil
	}
	var commands [][]string
	if access.groupMissing {
		commands = append(commands, []string{"groupadd", "--system", userGPUGroup})
	}
	if access.user != "" && !access.member {
		commands = append(commands, []string{"usermod", "-aG", userGPUGroup, access.user})
	}
	return commands
}

// shellQuote returns a POSIX-safe word for a printed command. Paths are the
// untrusted part; names and flags already passed validAccountName or are
// constant, so they are returned unchanged.
func shellQuote(word string) string {
	if word == "" {
		return "''"
	}
	for _, r := range word {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '_', r == '-', r == '.', r == '/', r == ':', r == '=', r == '+', r == '@', r == '%', r == '^', r == ',':
		default:
			return "'" + strings.ReplaceAll(word, "'", `'\''`) + "'"
		}
	}
	return word
}

// commandLine renders one argv with every word shell-quoted.
func commandLine(argv []string) string {
	quoted := make([]string, len(argv))
	for i, word := range argv {
		quoted[i] = shellQuote(word)
	}
	return strings.Join(quoted, " ")
}

// printRootCommands prints the sudo commands the install will run, so the
// operator can copy them on the no-terminal path.
func printRootCommands(deps Deps, commands [][]string) {
	for _, argv := range commands {
		fmt.Fprintf(deps.Stdout, "  sudo %s\n", commandLine(argv))
	}
}

// runRootCommands prints and then runs root-owned commands. It returns true
// only when every command ran. Without a terminal, or when sudo fails or is
// declined, it prints the commands for the operator and returns false; the
// caller then leaves config and manifest unchanged.
func runRootCommands(ctx context.Context, deps Deps, commands [][]string) bool {
	printRootCommands(deps, commands)
	if !deps.stdinIsTerminal() {
		fmt.Fprintln(deps.Stdout, "gpu registers: no terminal, so these commands were not run; run them by hand.")
		return false
	}
	for index, argv := range commands {
		if err := deps.runSudo(ctx, argv[0], argv[1:]...); err != nil {
			fmt.Fprintf(deps.Stdout, "gpu registers: sudo %s failed: %v\n", commandLine(argv), err)
			if index < len(commands)-1 {
				fmt.Fprintln(deps.Stdout, "gpu registers: the remaining commands were not run; run the commands above by hand.")
			}
			return false
		}
	}
	return true
}

// userGPURuleDraftName is the rendered rule kept in the installation state
// directory until the operator runs the printed commands, or until a terminal
// install succeeds and deletes it.
const userGPURuleDraftName = "gpu-registers.rules"

// writeGPURuleTemp writes the rendered rule into the installation's own state
// directory. The file is user-owned and persistent, so a no-terminal install
// leaves a path the operator can run the printed command against after the
// shell that started the install is gone. It holds no secret, so it is 0644: a
// user home on a root-squashed NFS export must still let the root-run install
// read it once copied to a local path.
func writeGPURuleTemp(paths config.Paths, content string) (string, func(), error) {
	path := filepath.Join(paths.StateDir, userGPURuleDraftName)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		return "", func() {}, fmt.Errorf("write the rendered GPU rule to %s: %w", path, err)
	}
	return path, func() { _ = os.Remove(path) }, nil
}

// setupUserGPURegisters prepares and verifies NVIDIA BAR0 access for a
// user-scope install. It sets cfg.Components.Collector.Hardware.GPURegisters
// only when at least one NVIDIA display device can be probed as the invoking
// user and, for the shared group, only when the systemd --user manager is not
// known to lack that group. A manager that cannot be checked is reported and a
// successful probe still enables sampling. Hardware and permission problems are
// reported and leave the feature off; only a manifest write failure aborts the
// installation.
func setupUserGPURegisters(ctx context.Context, deps Deps, paths config.Paths, m *manifest, cfg *config.Config) error {
	rulePath := paths.UdevGPUTempsFile
	if rulePath == "" {
		return errors.New("user installation paths are incomplete")
	}
	disable := func() { cfg.Components.Collector.Hardware.GPURegisters = false }

	current, readErr := os.ReadFile(rulePath)
	exists := readErr == nil
	if readErr != nil && !errors.Is(readErr, fs.ErrNotExist) {
		disable()
		deps.warnf(deps.Stdout, "cannot inspect %s: %v\ngpu registers: not enabled", rulePath, readErr)
		return nil
	}
	entry := m.entry(rulePath, kindUdevGPU)
	if entry == nil {
		return fmt.Errorf("internal error: %s is not part of the user installation manifest", rulePath)
	}

	access := planUserGPUAccess(deps)
	if !validAccountName(access.group) || !validAccountName(access.user) {
		disable()
		deps.warnf(deps.Stdout, "cannot determine a group for GPU register access; leaving it off")
		return nil
	}

	// A rule this installation recorded as owned is its own only while the
	// file still matches the recorded digest and grants the recorded group. A
	// system install, another account or an operator may have rewritten it
	// since; then it belongs to whoever wrote it and this install only adopts
	// it, so uninstall can never delete a rule another install now needs.
	if exists && entry.State == manifestOwned && !ownsUserGPURule(entry, current) {
		if err := markPathAdopted(m, paths, rulePath, kindUdevGPU); err != nil {
			return err
		}
	}

	// A rule that grants another account's private group, or that carries no
	// group Rigscope can recognise, is never taken over: rewriting it would
	// steal access from its owner.
	existingGroup := ""
	sharedAdopt := false
	if exists {
		existingGroup = ruleGroup(string(current))
		managed := systemd.IsManaged(string(current)) || systemd.IsLegacyManaged(string(current))
		switch {
		case existingGroup == access.group:
			// Already grants what this install needs.
		case access.privateCandidate != "" && existingGroup == access.privateCandidate:
			// This account's own earlier private group. NSS may have forced
			// the plan to the shared group; rewriting is still allowed.
		case existingGroup == userGPUGroup && access.private:
			// A system install or 0.2 owns the shared rule. Adopt it and join
			// the shared group instead of rewriting it to the private group.
			useSharedGroup(deps, &access)
			sharedAdopt = true
		case existingGroup == "" && managed:
			// A Rigscope rule with no parseable group; rewrite it below.
		default:
			disable()
			group := existingGroup
			if group == "" {
				group = "no recognisable group"
			}
			deps.warnf(deps.Stdout, "%s grants %s, which belongs to another account or is not Rigscope's; leaving it alone", rulePath, group)
			fmt.Fprintln(deps.Stdout, "gpu registers: not enabled")
			return nil
		}
	}

	// An existing rule that already grants the chosen group needs no rewrite.
	// The shared path still has to ensure the user is in the group; the
	// systemd manager check happens in verifyUserGPURegisters.
	if exists && existingGroup == access.group {
		if !access.private {
			if sharedAdopt {
				fmt.Fprintf(deps.Stdout, "gpu registers: using the existing shared-group rule at %s\n", rulePath)
			}
			commands := userGPUMembershipCommands(access)
			if len(commands) > 0 && !runRootCommands(ctx, deps, commands) {
				fmt.Fprintln(deps.Stdout, "gpu registers: run the commands above by hand, then log out fully")
			}
		}
		return adoptUserGPURule(deps, paths, m, cfg, access, rulePath, string(current), sharedAdopt)
	}

	content := systemd.RenderGPUTempsRule(access.group)
	tempPath, cleanup, err := writeGPURuleTemp(paths, content)
	if err != nil {
		return err
	}

	fmt.Fprintf(deps.Stdout, "gpu registers: NVIDIA BAR0 read access needs a root-owned udev rule\n")
	fmt.Fprintf(deps.Stdout, "  rule:  %s\n", rulePath)
	if access.private {
		fmt.Fprintf(deps.Stdout, "  group: %s (your private primary group; access works immediately)\n", access.group)
	} else {
		fmt.Fprintf(deps.Stdout, "  group: %s (shared; a full log-out is required before the collector can read the registers)\n", userGPUGroup)
	}
	commands := userGPUInstallCommands(access, tempPath, rulePath)
	if !runRootCommands(ctx, deps, commands) {
		// The printed install command names the rendered rule, so keep it for
		// the operator instead of deleting the file the command reads.
		fmt.Fprintf(deps.Stdout, "gpu registers: the rendered rule was kept at %s\n", shellQuote(tempPath))
		disable()
		deps.warnf(deps.Stdout, "GPU register access was not enabled; the rest of the installation is unaffected")
		return nil
	}
	cleanup()

	if err := recordInstalledUserGPURule(m, paths, rulePath, access); err != nil {
		return err
	}
	return verifyUserGPURegisters(deps, paths, m, cfg, access, rulePath, false)
}

// ownsUserGPURule reports whether the file on disk is still the one this
// installation recorded: same digest and same group. An entry without either
// record cannot prove ownership.
func ownsUserGPURule(entry *manifestEntry, content []byte) bool {
	if entry == nil || entry.State != manifestOwned || entry.SHA256 == "" || entry.RuleGroup == "" {
		return false
	}
	return fsutil.SHA256Bytes(content) == entry.SHA256 && ruleGroup(string(content)) == entry.RuleGroup
}

// recordOwnedUserGPURule records a private-group rule this installation wrote
// and the group it grants. Uninstall requires both, so a later rewrite for
// another group is never mistaken for this installation's file.
func recordOwnedUserGPURule(m *manifest, paths config.Paths, rulePath, group string) error {
	if err := markPathOwned(m, paths, rulePath, kindUdevGPU); err != nil {
		return err
	}
	entry := m.entry(rulePath, kindUdevGPU)
	if entry == nil {
		return fmt.Errorf("internal error: %s is not part of the installation manifest", rulePath)
	}
	entry.RuleGroup = group
	return writeManifest(paths.ManifestFile(), m)
}

// recordInstalledUserGPURule records a rule this install just applied. Only a
// rule for the user's private group may be owned. A rigscope-gpu rule written
// by a user install is adopted, because other installations and accounts rely
// on it: uninstall leaves a shared rule in place.
func recordInstalledUserGPURule(m *manifest, paths config.Paths, rulePath string, access userGPUAccess) error {
	if access.private {
		return recordOwnedUserGPURule(m, paths, rulePath, access.group)
	}
	return markPathAdopted(m, paths, rulePath, kindUdevGPU)
}

// adoptUserGPURule records an existing rule and verifies access. A byte-identical
// rule for the user's own private group is recorded as owned, so a rule the
// operator installed by hand from the printed commands is removed by uninstall.
// Everything else is adopted and left alone: a shared rigscope-gpu rule belongs
// to every account in the group, so no single user install claims it, even when
// that install wrote it.
func adoptUserGPURule(deps Deps, paths config.Paths, m *manifest, cfg *config.Config, access userGPUAccess, rulePath, content string, sharedAdopt bool) error {
	entry := m.entry(rulePath, kindUdevGPU)
	if entry == nil {
		return fmt.Errorf("internal error: %s is not part of the user installation manifest", rulePath)
	}
	owned := !sharedAdopt && access.private && systemd.IsManaged(content) &&
		content == systemd.RenderGPUTempsRule(access.group)
	if owned {
		if err := recordOwnedUserGPURule(m, paths, rulePath, access.group); err != nil {
			return err
		}
	} else if err := markPathAdopted(m, paths, rulePath, kindUdevGPU); err != nil {
		return err
	}
	if err := verifyUserGPURegisters(deps, paths, m, cfg, access, rulePath, true); err != nil {
		return err
	}
	if cfg.Components.Collector.Hardware.GPURegisters {
		fmt.Fprintf(deps.Stdout, "gpu registers: using the existing rule at %s (%d GPU(s) readable)\n", rulePath, probeReadable(deps))
	}
	if !owned {
		fmt.Fprintf(deps.Stdout, "gpu registers: the rule at %s is recorded as adopted; uninstall will leave it in place\n", rulePath)
	}
	return nil
}

// probeReadable reports how many NVIDIA display functions this session can map.
// It exists only for the adoption message, which runs after the fuller
// verification above.
func probeReadable(deps Deps) int {
	return probeGPUDisplays(deps).readable
}

// readerName names the account the readability probe ran as.
func readerName(deps Deps) string {
	if me, err := deps.accounts().Current(); err == nil && me.Name != "" {
		return me.Name
	}
	return "the current user"
}

// verifyUserGPURegisters enables register sampling only when BAR0 is readable
// now and, for the shared group, when the systemd --user manager carries the
// group. That manager inherits supplementary groups once, at start, so a probe
// from a fresh shell does not prove the collector has them. In the private
// case the primary group is always active and needs no check.
func verifyUserGPURegisters(deps Deps, paths config.Paths, m *manifest, cfg *config.Config, access userGPUAccess, rulePath string, existing bool) error {
	probe := probeGPUDisplays(deps)
	if probe.readable > 0 {
		if !access.private {
			missing, known := managerMissingGroup(deps, access.group)
			if known && missing {
				cfg.Components.Collector.Hardware.GPURegisters = false
				printUserManagerGroupNotice(deps, access)
				return nil
			}
			if !known {
				// Enabling on a successful probe is the documented behaviour
				// when the manager cannot be inspected, but say so instead of
				// pretending the collector's session was checked.
				deps.warnf(deps.Stdout, "could not check whether the collector's systemd --user session carries the %s group", access.group)
				fmt.Fprintln(deps.Stdout, "  the registers are readable from this session, so register sampling is enabled; if the session started before the group was added, log out fully and rerun: rigscope install --gpu-registers")
			}
		}
		cfg.Components.Collector.Hardware.GPURegisters = true
		fmt.Fprintf(deps.Stdout, "gpu registers: enabled (%d GPU(s) readable by %s)\n", probe.readable, readerName(deps))
		return nil
	}
	cfg.Components.Collector.Hardware.GPURegisters = false

	entry := m.entry(rulePath, kindUdevGPU)
	if entry != nil && entry.State == manifestOwned {
		if err := markPathOwned(m, paths, rulePath, kindUdevGPU); err != nil {
			return err
		}
	}
	state := "rule installed"
	if existing {
		state = "rule present"
	}
	switch {
	case probe.devices == 0:
		fmt.Fprintf(deps.Stdout, "gpu registers: no NVIDIA display device found under %s; leaving register sampling off\n", deps.hostPath("sys", "bus", "pci", "devices"))
	case probe.suspended > 0 && probe.suspended == probe.devices:
		fmt.Fprintf(deps.Stdout, "gpu registers: %d NVIDIA display device(s) are present but suspended; they were not probed, so register sampling stays off\n", probe.suspended)
		fmt.Fprintln(deps.Stdout, "  retry when a GPU is active: rigscope install --gpu-registers")
	case probe.denied && !access.private:
		fmt.Fprintf(deps.Stdout, "gpu registers: %s, but the registers are not readable yet.\n", state)
		fmt.Fprintf(deps.Stdout, "  Log out fully (not just the desktop session) so the user manager picks up the %s group,\n", userGPUGroup)
		fmt.Fprintf(deps.Stdout, "  then rerun: rigscope install --gpu-registers\n")
		if deps.lingerEnabled(access.user) {
			fmt.Fprintf(deps.Stdout, "  Linger keeps the old manager running across log-out: reboot, or run loginctl terminate-user %s\n", shellQuote(access.user))
		}
	case probe.denied:
		fmt.Fprintf(deps.Stdout, "gpu registers: %s, but BAR0 is still not readable (permission denied)\n", state)
		fmt.Fprintf(deps.Stdout, "  If access was just granted, run: sudo udevadm trigger --subsystem-match=pci --attr-match=vendor=0x10de\n")
	case probe.err != nil:
		fmt.Fprintf(deps.Stdout, "gpu registers: %s, but BAR0 is not readable: %v\n", state, probe.err)
	default:
		fmt.Fprintf(deps.Stdout, "gpu registers: %s, but no register could be read\n", state)
	}
	if !existing {
		fmt.Fprintln(deps.Stdout, "gpu registers: register temperatures stay off until a read succeeds")
	}
	return nil
}

// managerMissingGroup reports whether the invoking user's systemd --user
// manager is known to lack group. known is false when the manager cannot be
// found or the group cannot be resolved through the account database; the
// caller then cannot vouch for the collector's session but does not treat it as
// missing.
func managerMissingGroup(deps Deps, group string) (missing, known bool) {
	gid, resolved := groupGID(deps.accounts(), group)
	if !resolved {
		return false, false
	}
	groups, err := deps.userManagerGroups()
	if err != nil {
		return false, false
	}
	for _, managed := range groups {
		if managed == gid {
			return false, true
		}
	}
	return true, true
}

// printUserManagerGroupNotice tells the user why a readable session is not yet
// enough, and prints (never runs) the command that also fixes linger.
func printUserManagerGroupNotice(deps Deps, access userGPUAccess) {
	fmt.Fprintf(deps.Stdout, "gpu registers: the registers are readable by %s in this session, but the systemd --user manager does not have the %s group yet.\n", readerName(deps), access.group)
	fmt.Fprintln(deps.Stdout, "  Log out of all sessions (not just the desktop session) so the user manager starts with the group,")
	fmt.Fprintln(deps.Stdout, "  then rerun: rigscope install --gpu-registers")
	if deps.lingerEnabled(access.user) {
		fmt.Fprintln(deps.Stdout, "  Linger keeps the old manager running across log-out, so reboot, or run:")
		fmt.Fprintf(deps.Stdout, "    loginctl terminate-user %s\n", shellQuote(access.user))
	}
	fmt.Fprintln(deps.Stdout, "gpu registers: left off until the collector's user manager has the group")
}

// removeUserGPURule removes the root-owned GPU rule recorded by a user
// installation, but only while every proof of ownership still holds: the entry
// is owned, the file carries the Rigscope marker, its content still matches the
// recorded digest, and it grants the user's private group. Anything else is
// left in place with the command to remove it, because another install may have
// taken the rule over. Failed cleanup aborts deletion of the local installation
// so its binary and ownership record remain available for a retry.
func removeUserGPURule(ctx context.Context, deps Deps, paths config.Paths, current *manifest) error {
	if paths.Scope != config.ScopeUser || current == nil {
		return nil
	}
	rulePath := paths.UdevGPUTempsFile
	entry := current.entry(rulePath, kindUdevGPU)
	if entry == nil {
		return nil
	}
	content, err := os.ReadFile(rulePath)
	if errors.Is(err, fs.ErrNotExist) {
		// A previous attempt may have removed the file but failed to reload
		// udev. Complete that step before dropping the ownership record.
		if entry.State == manifestOwned && !runRootCommands(ctx, deps, [][]string{{"udevadm", "control", "--reload"}}) {
			return fmt.Errorf("could not reload udev after removing %s", rulePath)
		}
		return nil
	}
	if err != nil {
		deps.warnf(deps.Stderr, "could not read %s: %v", rulePath, err)
		printLeftGPURule(deps, rulePath, "it could not be read")
		return fmt.Errorf("read GPU rule: %w", err)
	}
	if reason := userGPURuleUnownable(deps, entry, content); reason != "" {
		if !systemd.IsManaged(string(content)) {
			deps.warnf(deps.Stderr, "refusing to remove unrecognized file %s; inspect it first", rulePath)
		}
		printLeftGPURule(deps, rulePath, reason)
		return nil
	}
	commands := [][]string{
		{"rm", "-f", rulePath},
		{"udevadm", "control", "--reload"},
	}
	if !runRootCommands(ctx, deps, commands) {
		return fmt.Errorf("could not complete GPU rule removal and udev reload for %s", rulePath)
	}
	fmt.Fprintf(deps.Stdout, "  gpu rule: removed %s\n", rulePath)
	return nil
}

// userGPURuleUnownable explains why an existing rule must not be removed, or
// returns "" when it is safe to delete.
func userGPURuleUnownable(deps Deps, entry *manifestEntry, content []byte) string {
	if entry.State != manifestOwned {
		return "it was not written by this installation"
	}
	if !systemd.IsManaged(string(content)) {
		return "its content no longer carries the Rigscope marker"
	}
	if entry.SHA256 == "" || entry.RuleGroup == "" {
		return "the installation manifest does not record what it wrote"
	}
	if fsutil.SHA256Bytes(content) != entry.SHA256 {
		return "its content changed since install recorded it"
	}
	private, ok := privatePrimaryGroup(deps.accounts())
	if !ok {
		return "your private group cannot be determined"
	}
	if entry.RuleGroup != private {
		return fmt.Sprintf("it was recorded for group %s, not your private group", entry.RuleGroup)
	}
	if group := ruleGroup(string(content)); group != private {
		reason := fmt.Sprintf("it now grants group %s, not your private group", group)
		if group == "" {
			reason = "it no longer names a group Rigscope can recognise"
		}
		return reason
	}
	return ""
}

// printLeftGPURule says why a rule was kept and prints (never runs) the command
// that removes it.
func printLeftGPURule(deps Deps, rulePath, reason string) {
	fmt.Fprintf(deps.Stdout, "  gpu rule: left in place (%s)\n", reason)
	fmt.Fprintln(deps.Stdout, "            remove it by hand with:")
	fmt.Fprintf(deps.Stdout, "              sudo rm -f %s\n", shellQuote(rulePath))
	fmt.Fprintln(deps.Stdout, "              sudo udevadm control --reload")
}
