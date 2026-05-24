package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/gastownhall/gascity/internal/config"
	"github.com/spf13/cobra"
)

// bdSilentFallbackExitCode is the exit code gc bd emits when it detects
// that bd silently fell back to on-disk auto-import mode (managed Dolt
// unreachable). Distinct from bd's own exits so operators and CI can
// tell the loud-fail apart from a real bd error. Covers both the
// bd update path (gastownhall/gascity#2080) and the bd close path
// (gastownhall/gascity#2079) because both subcommands flow through doBd.
const bdSilentFallbackExitCode = 4

// bdStderrScanLimit caps how much of bd's stderr gc retains to scan for the
// silent-fallback marker. bd emits the marker pair while opening the store —
// before it runs the subcommand — so the marker, when present, always lands
// within the first chunk of stderr. Capping the retained prefix keeps memory
// bounded for bd subcommands that stream large stderr output.
const bdStderrScanLimit = 64 << 10 // 64 KiB

// bdNoteCommentChunkLimit keeps fallback comment bodies safely below Dolt's
// TEXT column limit while preserving large evidence bundles across multiple
// structured comments.
const bdNoteCommentChunkLimit = 60 << 10 // 60 KiB

// headLimitedWriter retains only the first limit bytes written to it and
// discards the rest, so scanning bd's stderr for the silent-fallback marker
// never holds an unbounded copy of the stream. It always reports a full
// write so it is safe as an io.MultiWriter sink.
type headLimitedWriter struct {
	buf   []byte
	limit int
}

func (w *headLimitedWriter) Write(p []byte) (int, error) {
	if room := w.limit - len(w.buf); room > 0 {
		if len(p) < room {
			room = len(p)
		}
		w.buf = append(w.buf, p[:room]...)
	}
	return len(p), nil
}

func (w *headLimitedWriter) String() string { return string(w.buf) }

type bdOversizedNoteFallback struct {
	beadID       string
	noteText     string
	retryArgs    []string
	shouldRetry  bool
	originalFlag string
}

func newBdCmd(stdout, stderr io.Writer) *cobra.Command {
	cmd := &cobra.Command{
		Use:   "bd [bd-args...]",
		Short: "Run bd in the correct rig directory",
		Long: `Run a bd command routed to the correct rig directory.

When beads belong to a rig (not the city root), bd must run from the
rig directory to find the correct .beads database. This command resolves
the rig automatically from the --rig flag or by detecting the bead prefix
in the arguments.

All arguments after "gc bd" are forwarded to bd unchanged.

gc bd forces BD_EXPORT_AUTO=false to prevent bd's git auto-export hook
from wedging the wrapper after printing command output. If you need
auto-export behavior, invoke bd directly.

When upstream bd rejects a notes write because its audit event would exceed
Dolt TEXT limits, gc bd retries any non-note update flags and stores the note
body as chunked comments so operational evidence is not lost.`,
		Example: `  gc bd --rig my-project list
  gc bd --rig my-project create "New task"
  gc bd show my-project-abc          # auto-detects rig from bead prefix
  gc bd list --rig my-project -s open`,
		DisableFlagParsing: true,
		RunE: func(_ *cobra.Command, args []string) error {
			// Plumb doBd's numeric exit code through exitForCode so the
			// process exit code matches the documented contract above
			// (bdSilentFallbackExitCode = 4) and bd's own exit codes are
			// preserved. Returning errExit on any non-zero would collapse
			// every code to 1 and defeat the operator/CI signal the loud-
			// fail was meant to provide.
			return exitForCode(doBd(args, stdout, stderr))
		},
	}
	return cmd
}

var bdBeadExists = func(cityPath string, target execStoreTarget, beadID string) bool {
	store, err := openStoreAtForCity(target.ScopeRoot, cityPath)
	if err != nil {
		return false
	}
	bead, err := store.Get(beadID)
	return err == nil && strings.TrimSpace(bead.ID) != ""
}

func bdCommandEnv(cityPath string, cfg *config.City, target execStoreTarget) ([]string, error) {
	var overrides map[string]string
	var err error
	if target.ScopeKind == "rig" {
		overrides, err = bdRuntimeEnvForRigWithError(cityPath, cfg, target.ScopeRoot)
	} else {
		overrides, err = bdRuntimeEnvWithError(cityPath)
	}
	if err != nil {
		return nil, err
	}
	if target.ScopeKind != "rig" {
		overrides["GC_RIG"] = ""
		overrides["GC_RIG_ROOT"] = ""
		overrides["BEADS_DIR"] = filepath.Join(target.ScopeRoot, ".beads")
	}
	overrides["GC_STORE_ROOT"] = target.ScopeRoot
	overrides["GC_STORE_SCOPE"] = target.ScopeKind
	overrides["GC_BEADS_PREFIX"] = target.Prefix
	applyExportSuppressionEnv(overrides)
	return mergeRuntimeEnv(os.Environ(), overrides), nil
}

func warnExternalBdOverrideDrift(stderr io.Writer, cityPath string, target execStoreTarget) {
	resolved, ok, err := canonicalScopeDoltTarget(cityPath, target.ScopeRoot)
	if err != nil || !ok || !resolved.External {
		return
	}
	var drift []string
	if host := strings.TrimSpace(os.Getenv("GC_DOLT_HOST")); host != "" && host != strings.TrimSpace(resolved.Host) {
		drift = append(drift, fmt.Sprintf("GC_DOLT_HOST=%s (canonical %s)", host, strings.TrimSpace(resolved.Host)))
	}
	if port := strings.TrimSpace(os.Getenv("GC_DOLT_PORT")); port != "" && port != strings.TrimSpace(resolved.Port) {
		drift = append(drift, fmt.Sprintf("GC_DOLT_PORT=%s (canonical %s)", port, strings.TrimSpace(resolved.Port)))
	}
	if len(drift) == 0 {
		return
	}
	_, _ = fmt.Fprintf(stderr, "gc bd: warning: ignoring ambient Dolt host/port override for external target: %s\n", strings.Join(drift, ", "))
}

func doBd(args []string, stdout, stderr io.Writer) int {
	cityName, rigName, bdArgs := extractBdScopeFlags(args)

	cityPath, err := resolveBdCity(cityName)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	// Use the full config load path (includes pack expansion + site
	// binding overlay) so migrated rigs (path only in .gc/site.toml)
	// resolve to their bound path. A raw config.Load here would make
	// every already-migrated rig look unbound and fail the new guard
	// in resolveBdScopeTarget / bdRigScopeTarget.
	cfg, err := loadCityConfig(cityPath, stderr)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: loading config: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}

	target, err := resolveBdScopeTarget(cfg, cityPath, rigName, bdArgs)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	if provider := rawBeadsProviderForScope(target.ScopeRoot, cityPath); !providerUsesBdStoreContract(provider) {
		fmt.Fprintf(stderr, "gc bd: only supported for bd-backed beads providers (resolved %q for %s)\n", provider, target.ScopeRoot) //nolint:errcheck // best-effort stderr
		if hint := bdProviderMismatchHint(target.ScopeRoot, provider); hint != "" {
			fmt.Fprintf(stderr, "  hint: %s\n", hint) //nolint:errcheck // best-effort stderr
		}
		return 1
	}

	reapStaleBdExportJSONL(target.ScopeRoot)
	warnExternalBdOverrideDrift(stderr, cityPath, target)

	bdPath, err := exec.LookPath("bd")
	if err != nil {
		fmt.Fprintln(stderr, "gc bd: bd not found in PATH") //nolint:errcheck // best-effort stderr
		return 1
	}

	cmd := exec.Command(bdPath, bdArgs...)
	cmd.Dir = target.ScopeRoot
	cmd.Stdin = os.Stdin
	cmd.Stdout = stdout
	// Tee stderr through a bounded head buffer alongside the operator's
	// pipe so we can scan it post-exec for bd's silent-fallback-to-on-disk
	// marker. Only stderr is teed: bd writes its auto-import banner there,
	// not to stdout. See gastownhall/gascity#2080 (update path) and #2079
	// (close path) — both go through this handoff.
	stderrScan := &headLimitedWriter{limit: bdStderrScanLimit}
	cmd.Stderr = io.MultiWriter(stderr, stderrScan)
	env, err := bdCommandEnv(cityPath, cfg, target)
	if err != nil {
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	cmdEnv := workQueryEnvForDir(env, cmd.Dir)
	cmd.Env = cmdEnv

	runErr := cmd.Run()

	if runErr != nil {
		var exitErr *exec.ExitError
		if errors.As(runErr, &exitErr) {
			if fallback, ok := bdOversizedNoteFallbackForArgs(bdArgs, stderrScan.String()); ok {
				code := runBdOversizedNoteFallback(bdPath, target.ScopeRoot, cmdEnv, fallback, stdout, stderr)
				if code == 0 {
					return 0
				}
				return code
			}
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "gc bd: %v\n", runErr) //nolint:errcheck // best-effort stderr
		return 1
	}

	// bd exited 0 — but if its stderr shows the silent fallback to on-disk
	// auto-import, the managed Dolt server was unreachable and any write in
	// this command was dropped (managed Gas City sets BD_EXPORT_AUTO=false;
	// see applyExportSuppressionEnv in cmd/gc/bd_env.go). Surface that as a
	// hard error instead of a misleading exit 0. One check here covers the
	// whole bd-write-persistence quad (gastownhall/gascity#2079 / #2080 /
	// #2149 / #2150) because every bd subcommand routes through this
	// handoff. A non-zero bd exit is intentionally left to the block above:
	// the existing transport-retry classifier already handles the
	// timeout+marker case, and overriding a real bd exit code here would
	// mask it. (Root cause fixed upstream in beads post-#3691; this surfaces
	// the symptom for deployments still on stable bd builds.)
	if bdOutputIndicatesSilentFallback(stderrScan.String()) {
		fmt.Fprintln(stderr, "gc bd: managed Dolt unreachable; bd fell back to on-disk auto-import mode. If this command wrote data, that write was NOT persisted. Restart the managed Dolt server (or check connectivity) and retry. (See gastownhall/gascity#2080.)") //nolint:errcheck // best-effort stderr
		return bdSilentFallbackExitCode
	}

	return 0
}

func bdOversizedNoteFallbackForArgs(args []string, bdOutput string) (bdOversizedNoteFallback, bool) {
	if !bdOutputIndicatesOversizedEventValue(bdOutput) || len(args) == 0 {
		return bdOversizedNoteFallback{}, false
	}
	switch args[0] {
	case "update":
		return parseBdUpdateOversizedNoteFallback(args)
	case "note":
		return parseBdNoteOversizedNoteFallback(args)
	default:
		return bdOversizedNoteFallback{}, false
	}
}

func bdOutputIndicatesOversizedEventValue(output string) bool {
	msg := strings.ToLower(output)
	if !strings.Contains(msg, "old_value") && !strings.Contains(msg, "new_value") {
		return false
	}
	return strings.Contains(msg, "too large") || strings.Contains(msg, "data too long")
}

func parseBdUpdateOversizedNoteFallback(args []string) (bdOversizedNoteFallback, bool) {
	if len(args) < 2 || args[0] != "update" {
		return bdOversizedNoteFallback{}, false
	}

	retryArgs := []string{"update"}
	var ids []string
	var noteText string
	var originalFlag string
	usesStdin := false

	for i := 1; i < len(args); i++ {
		arg := args[i]
		if flag, value, ok := splitBdNoteFlag(arg); ok {
			if originalFlag != "" {
				return bdOversizedNoteFallback{}, false
			}
			originalFlag = flag
			noteText = value
			continue
		}
		if arg == "--notes" || arg == "--append-notes" {
			if originalFlag != "" || i+1 >= len(args) {
				return bdOversizedNoteFallback{}, false
			}
			originalFlag = arg
			noteText = args[i+1]
			i++
			continue
		}

		retryArgs = append(retryArgs, arg)

		if arg == "--stdin" {
			usesStdin = true
			continue
		}
		if strings.HasPrefix(arg, "--") {
			name, hasInlineValue := bdLongFlagName(arg)
			if hasInlineValue && (arg == "--body-file=-" || arg == "--description-file=-") {
				usesStdin = true
			}
			if bdUpdateFlagTakesValue(name) && !hasInlineValue {
				if i+1 >= len(args) {
					return bdOversizedNoteFallback{}, false
				}
				value := args[i+1]
				retryArgs = append(retryArgs, value)
				if (name == "body-file" || name == "description-file") && value == "-" {
					usesStdin = true
				}
				i++
			}
			continue
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			name := strings.TrimLeft(arg, "-")
			if bdUpdateShortFlagTakesValue(name) {
				if i+1 >= len(args) {
					return bdOversizedNoteFallback{}, false
				}
				retryArgs = append(retryArgs, args[i+1])
				i++
			}
			continue
		}
		ids = append(ids, arg)
	}

	if usesStdin || originalFlag == "" || noteText == "" || len(ids) != 1 {
		return bdOversizedNoteFallback{}, false
	}
	return bdOversizedNoteFallback{
		beadID:       ids[0],
		noteText:     noteText,
		retryArgs:    retryArgs,
		shouldRetry:  bdUpdateArgsHaveMutation(retryArgs),
		originalFlag: originalFlag,
	}, true
}

func parseBdNoteOversizedNoteFallback(args []string) (bdOversizedNoteFallback, bool) {
	if len(args) < 3 || args[0] != "note" || strings.HasPrefix(args[1], "-") {
		return bdOversizedNoteFallback{}, false
	}
	for _, arg := range args[2:] {
		if arg == "--stdin" || arg == "--file" || strings.HasPrefix(arg, "--file=") {
			return bdOversizedNoteFallback{}, false
		}
	}
	noteText := strings.Join(args[2:], " ")
	if noteText == "" {
		return bdOversizedNoteFallback{}, false
	}
	return bdOversizedNoteFallback{
		beadID:       args[1],
		noteText:     noteText,
		originalFlag: "note",
	}, true
}

func splitBdNoteFlag(arg string) (string, string, bool) {
	for _, flag := range []string{"--notes=", "--append-notes="} {
		if strings.HasPrefix(arg, flag) {
			return strings.TrimSuffix(flag, "="), strings.TrimPrefix(arg, flag), true
		}
	}
	return "", "", false
}

func bdLongFlagName(arg string) (string, bool) {
	trimmed := strings.TrimPrefix(arg, "--")
	name, _, hasValue := strings.Cut(trimmed, "=")
	return name, hasValue
}

func bdUpdateFlagTakesValue(name string) bool {
	switch name {
	case "status", "priority", "title", "type", "assignee", "description", "body", "message",
		"body-file", "description-file", "design", "design-file", "acceptance", "external-ref",
		"spec-id", "acceptance-criteria", "estimate", "add-label", "remove-label", "set-labels",
		"parent", "session", "due", "defer", "await-id", "metadata", "set-metadata", "unset-metadata":
		return true
	default:
		return false
	}
}

func bdUpdateShortFlagTakesValue(name string) bool {
	switch name {
	case "s", "a", "d", "m", "t", "e":
		return true
	default:
		return false
	}
}

func bdUpdateArgsHaveMutation(args []string) bool {
	for _, arg := range args[1:] {
		if strings.HasPrefix(arg, "--") {
			name, _ := bdLongFlagName(arg)
			switch name {
			case "json", "help":
				continue
			default:
				return true
			}
		}
		if strings.HasPrefix(arg, "-") && arg != "-" {
			name := strings.TrimLeft(arg, "-")
			if name != "h" {
				return true
			}
		}
	}
	return false
}

func runBdOversizedNoteFallback(bdPath, dir string, env []string, fallback bdOversizedNoteFallback, stdout, stderr io.Writer) int {
	fmt.Fprintf(stderr, "gc bd: bd could not write %s because Dolt rejected the audit old_value/new_value size; ", fallback.originalFlag) //nolint:errcheck // best-effort stderr
	fmt.Fprintln(stderr, "retrying non-note updates and storing the note as chunked comments.")                                           //nolint:errcheck // best-effort stderr
	if fallback.shouldRetry {
		if code := runBdSubcommand(bdPath, fallback.retryArgs, dir, env, nil, stdout, stderr); code != 0 {
			return code
		}
	}
	chunks := chunkStringByBytes(fallback.noteText, bdNoteCommentChunkLimit)
	for _, chunk := range chunks {
		if code := runBdSubcommand(bdPath, []string{"comment", fallback.beadID, "--stdin"}, dir, env, strings.NewReader(chunk), stdout, stderr); code != 0 {
			return code
		}
	}
	return 0
}

func runBdSubcommand(bdPath string, args []string, dir string, env []string, stdin io.Reader, stdout, stderr io.Writer) int {
	cmd := exec.Command(bdPath, args...)
	cmd.Dir = dir
	if stdin != nil {
		cmd.Stdin = stdin
	} else {
		cmd.Stdin = os.Stdin
	}
	cmd.Stdout = stdout
	cmd.Stderr = stderr
	cmd.Env = env
	if err := cmd.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) {
			return exitErr.ExitCode()
		}
		fmt.Fprintf(stderr, "gc bd: %v\n", err) //nolint:errcheck // best-effort stderr
		return 1
	}
	return 0
}

func chunkStringByBytes(s string, limit int) []string {
	if limit <= 0 || len(s) <= limit {
		return []string{s}
	}
	chunks := make([]string, 0, (len(s)/limit)+1)
	start := 0
	size := 0
	for idx, r := range s {
		runeSize := utf8.RuneLen(r)
		if runeSize < 0 {
			runeSize = 1
		}
		if size > 0 && size+runeSize > limit {
			chunks = append(chunks, s[start:idx])
			start = idx
			size = 0
		}
		size += runeSize
	}
	if start < len(s) {
		chunks = append(chunks, s[start:])
	}
	return chunks
}

func resolveBdCity(cityName string) (string, error) {
	if strings.TrimSpace(cityName) != "" {
		return validateCityPath(cityName)
	}
	return resolveCity()
}

// extractBdScopeFlags extracts gc-owned --city/--rig flags from the raw
// argument list and returns the requested city, rig, and remaining bd args.
// It also falls back to cobra's persistent globals for "gc --city X --rig Y bd".
func extractBdScopeFlags(args []string) (string, string, []string) {
	var cityName string
	var rigName string
	var rest []string
	for i := 0; i < len(args); i++ {
		switch {
		case args[i] == "--city" && i+1 < len(args):
			cityName = args[i+1]
			i++
			continue
		case strings.HasPrefix(args[i], "--city="):
			cityName = strings.TrimPrefix(args[i], "--city=")
			continue
		case args[i] == "--rig" && i+1 < len(args):
			rigName = args[i+1]
			i++
			continue
		case strings.HasPrefix(args[i], "--rig="):
			rigName = strings.TrimPrefix(args[i], "--rig=")
			continue
		}
		rest = append(rest, args[i])
	}
	if cityName == "" && cityFlag != "" {
		cityName = cityFlag
	}
	if rigName == "" && rigFlag != "" {
		rigName = rigFlag
	}
	return cityName, rigName, rest
}

// extractRigFlag extracts --rig <name> from the argument list and returns
// the rig name and remaining args. Also checks the global rigFlag set by
// cobra's persistent flag parsing (for "gc --rig foo bd list" syntax).
func extractRigFlag(args []string) (string, []string) {
	_, rigName, rest := extractBdScopeFlags(args)
	return rigName, rest
}

// resolveBdScopeTarget determines the canonical scope root for a bd command.
// Priority: explicit rig name > bead prefix auto-detection > enclosing rig > city root.
func resolveBdScopeTarget(cfg *config.City, cityPath, rigName string, args []string) (execStoreTarget, error) {
	resolveRigPaths(cityPath, cfg.Rigs)
	if rigName != "" {
		rig, ok := rigByName(cfg, rigName)
		if !ok {
			return execStoreTarget{}, fmt.Errorf("rig %q not found", rigName)
		}
		if strings.TrimSpace(rig.Path) == "" {
			return execStoreTarget{}, fmt.Errorf("rig %q is declared but has no path binding — run `gc rig add <dir> --name %s` to bind it before scoping bd commands", rig.Name, rig.Name)
		}
		return bdRigScopeTarget(cityPath, rig), nil
	}

	cityTarget := bdCityScopeTarget(cityPath, cfg)
	cityPrefix := config.EffectiveHQPrefix(cfg)
	if cityPrefix != "" {
		for _, arg := range args {
			if strings.HasPrefix(arg, "-") || beadPrefix(cfg, arg) != cityPrefix {
				continue
			}
			if bdBeadExists(cityPath, cityTarget, arg) {
				return cityTarget, nil
			}
		}
	}

	// Auto-detect from bead IDs in args, but only accept candidates that
	// actually exist in the resolved rig store. This keeps hyphenated flag
	// values and other non-ID args from silently retargeting the command.
	// Unbound rigs are skipped so we don't alias them to the city store.
	for _, arg := range args {
		if strings.HasPrefix(arg, "-") {
			continue
		}
		if rig, ok := bdRigForArg(cfg, arg); ok {
			if strings.TrimSpace(rig.Path) == "" {
				continue
			}
			target := bdRigScopeTarget(cityPath, rig)
			if bdBeadExists(cityPath, target, arg) {
				return target, nil
			}
		}
	}

	if rig, ok, err := bdRigFromCwd(cfg, cityPath); err != nil {
		return execStoreTarget{}, err
	} else if ok {
		// resolveRigForDir already skips unbound rigs, so rig.Path is
		// guaranteed non-empty here.
		return bdRigScopeTarget(cityPath, rig), nil
	}

	return cityTarget, nil
}

func bdRigForArg(cfg *config.City, arg string) (config.Rig, bool) {
	if prefix := beadPrefix(cfg, arg); prefix != "" {
		return findRigByPrefix(cfg, prefix)
	}
	return config.Rig{}, false
}

func bdRigFromCwd(cfg *config.City, cityPath string) (config.Rig, bool, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return config.Rig{}, false, nil
	}
	return resolveRigForDir(cfg, cityPath, cwd)
}

func bdRigScopeTarget(cityPath string, rig config.Rig) execStoreTarget {
	return execStoreTarget{
		ScopeRoot: resolveStoreScopeRoot(cityPath, rig.Path),
		ScopeKind: "rig",
		Prefix:    rig.EffectivePrefix(),
		RigName:   rig.Name,
	}
}

func bdCityScopeTarget(cityPath string, cfg *config.City) execStoreTarget {
	return execStoreTarget{
		ScopeRoot: resolveStoreScopeRoot(cityPath, cityPath),
		ScopeKind: "city",
		Prefix:    config.EffectiveHQPrefix(cfg),
	}
}
