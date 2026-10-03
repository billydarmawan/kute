# Diagnostics and bug reports

A TUI owns the terminal, so it has nowhere to print. Everything client-go
would normally log to stderr is discarded, because stderr *is* the screen —
which is why a crash used to leave a user with nothing to attach to a report.
This is where that output goes instead.

Three pieces, all in `internal/diag` and `internal/app/crash.go`:

| Piece | What it is |
| --- | --- |
| `--log-file <path>` | The error/klog stream, written to a file instead of discarded. |
| The crash report | A file written on any panic, naming the build, context, kind, screen and terminal size. |
| The crash footer | What the user sees on the restored terminal: the same facts, plus where the file is. |

## `--log-file`

```sh
kute --log-file /tmp/kute.log
```

Everything client-go logs — reflector and watch errors, client-side throttling
warnings, anything routed through `utilruntime.ErrorHandlers` — plus kute's own
diagnostic lines (startup, a `config.yaml` that didn't parse and what kute did
about it, every connection-state transition and its error) go to that file. It is opened append, mode `0600`, and its directory is created if
missing. An unwritable path is a startup error rather than a silent downgrade:
a user who asked for a log and got none has no way to tell.

The file carries context, cluster and namespace names. It is not secret
material, but it is not nothing either — the bug form says so. Every line is
scrubbed on the way in, to the file and the ring alike (`diag.Redact`): bearer
tokens, `token=`/`secret:`/`password=`-style values and JWT-shaped strings
become `[redacted]`, and the home directory becomes `~`. A connection error is
also capped at 1 KiB before it is logged, since for an exec credential plugin
it is the plugin's own stderr.

**Without the flag** the stream still fills a bounded in-memory ring (the last
300 lines), so a crash report from a run that had no `--log-file` still shows
what the cluster was doing just before. That is the whole reason the ring
exists; nothing else reads it.

## Crash reports

Any panic writes `kute-crash-<timestamp>.log` to `$XDG_STATE_HOME/kute/`
(`~/.local/state/kute/` by default), falling back to the OS temp dir if that
can't be written — a report the user can't find is barely better than none.
The file is created exclusively (`O_EXCL`, `0600`), never opened if it already
exists, so a symlink planted at that predictable name in a shared temp dir is
not followed; a taken name gets a random suffix instead.
With `--log-file` the same report is appended to the log too, so one
attachment carries both the crash and the log that led to it.

It holds the build (version, commit, date), the platform and Go version, the
terminal size and `$TERM`/`$COLORTERM`/`$TERM_PROGRAM`, the active context,
namespace, kind and screen, the connection phase, the panic value and stack,
and the tail of the log stream.

`$TERM_PROGRAM` is in there for the same reason platform is, and often
outranks it: `TERM` and `COLORTERM` describe capabilities the emulator
*claims*, and rendering bugs land where a specific emulator gets those claims
wrong. Any of the three that is unset is left out rather than reported empty,
which would read as a claim of its own.

The footer prints the same fields minus two: the wall clock (the report's
filename carries it, and the user just watched it happen) and the Go toolchain
(pinned in released binaries). Platform is deliberately its own field rather
than a tail on the Go version, because the footer keeps it — "does this
reproduce on Windows" is the first question a terminal-rendering bug raises.

### Where panics are caught

| Site | Caught by | Report has |
| --- | --- | --- |
| Root `Update`/`View`/`Init` | `app.crashCatcher`, which records and re-panics | value + stack |
| An informer goroutine | `utilruntime.PanicHandlers` | value + stack |
| A kute-owned goroutine (event forwarding, connect, update check, port-forward loops, health ping, probes, exec-plugin stderr drain) | `defer utilruntime.HandleCrash()` → the same `PanicHandlers` entry | value + stack |
| A `tea.Cmd` goroutine | Bubble Tea itself, before kute sees it | context only; the stack went to the terminal |

Neither goroutine row passes through Bubble Tea, so its recover never restores
the terminal. The `PanicHandlers` entry does it instead — `Program.Kill()`,
before printing the footer — so the footer lands on the normal screen and the
shell is left in cooked mode. A new `go` statement in `internal/app` or
`internal/kube` should defer `utilruntime.HandleCrash()` to join that row.

`crashCatcher` deliberately re-panics after recording. Bubble Tea's own
recover is what puts the terminal back into a usable state, and it can only do
that if the panic keeps travelling — all kute's frame adds is the file.

The third row is the one gap, and it is a property of Bubble Tea, not a
decision: `Program.Run` recovers command-goroutine panics internally and
surfaces only `ErrProgramPanic`. `reportProgramCrash` still writes a report for
it, saying the value is unavailable rather than implying the run ended
cleanly — the context, kind and terminal size in it are still most of what a
maintainer needs. Bubble Tea prints that panic's stack from its own goroutine,
after `Run` has already returned, so kute waits a moment (`cmdPanicSettle`)
before printing the footer, to keep it below the trace rather than inside it.

After any `ErrProgramPanic`, `run` still saves `state.json` but does not fold
the current location into it (`saveSessionState`): if the kind, filter or
screen the user was on is what crashed, restoring it would crash every launch
after this one.

### The live snapshot

Every field a report needs about *where the user was* is read on the update
loop (`liveState.sync`, called from `crashCatcher.Update`) and cached under a
mutex. The crashing goroutine reads that copy. Reading `Session.Location`
directly from a dying informer goroutine would be a race with the update loop,
which is the one thing a crash reporter must not add.

## Verifying the crash path

`KUTE_CRASH_TEST` makes the next key press panic on purpose:

```sh
KUTE_CRASH_TEST=1 kute --demo --log-file /tmp/kute.log
# press any key
```

That is the acceptance test for this feature — a deliberately-crashed build
producing a file a maintainer can diagnose from — and it is an environment
variable rather than a patched build so it stays runnable, and stays true.

## The bug form

`.github/ISSUE_TEMPLATE/bug_report.yml` asks for exactly the fields the crash
footer prints, in the same order, so a reporter can paste the footer straight
in. Keep the two in step: a form asking for something the footer doesn't print
is a question the user can't answer.
