package main

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/rezzminator/callmeter/internal/callmeter/cmdparse"
)

// scriptBodyParser is the cmdparse.Version that split cmdparse.StatusScriptBody
// out of cmdparse.StatusUnparsed for a python part.
const scriptBodyParser = 6

// The python parts the parse loop counts apart from their stored
// parse_status (see scriptBodyParser); neither is a stored value.
const (
	statusPythonUnparsedPre6   = "python-unparsed-pre6"
	statusPythonCodeUnresolved = "python-code-unresolved"
)

// endReasonNever is the sessions.end_reason `callmeter report` writes for a
// session quiet past quietAfterMS with no SessionEnd recorded
// (callmeter.EndReasonNever; this check reuses nothing of internal/callmeter).
const endReasonNever = "never"

// endReasonLost is the sessions.end_reason `callmeter report` writes for a
// session whose own SessionEnd hook ran and was lost before it recorded: a
// missed.log line of the wrapper or of a hook binary killed by a signal names
// the SessionEnd and the session (callmeter.EndReasonLost).
const endReasonLost = "lost"

// quietAfterMS is how long a session must be quiet before report-time recovery
// marks it ended (callmeter.QuietAfter's value, in ms).
const quietAfterMS int64 = 60 * 60 * 1000

// stopWrite bounds how long after a TaskStop Claude Code writes the error
// result of the call it cut (tens of ms live: 33 ms and 29 ms on 2026-10-02).
const stopWrite int64 = 5_000 // ms

// settle is how long after its last line a sub-agent's turn is expected closed.
const settle = 60_000

// reconcile loads the snapshot and the transcripts and runs every comparison.
func reconcile(ctx context.Context, cfg config) (*report, error) {
	st, err := loadStore(ctx, cfg.db)
	if err != nil {
		return nil, err
	}
	if cfg.until == 0 {
		cfg.until = st.latest
	}
	if !cfg.sinceSet {
		cfg.since = st.earliest
	}
	if cfg.until < cfg.since {
		return nil, fmt.Errorf("the window ends (%s) before it starts (%s)", msString(cfg.until), msString(cfg.since))
	}
	index, err := diskIndex(cfg.projects)
	if err != nil {
		return nil, err
	}
	allow, err := loadAllowlist(cfg.allowlist)
	if err != nil {
		return nil, err
	}
	cfg.scratchStores, cfg.scratchRead, cfg.scratchUnreadable, err = loadScratch(ctx, cfg.scratch, cfg.db)
	if err != nil {
		return nil, err
	}
	// The evidence pass applies its own copy, so a line counts as used only by
	// a row this window judges.
	evidenceAllow := slices.Clone(allow)
	inWindow := func(s *sSession) bool { return s.firstTS >= cfg.since && s.firstTS <= cfg.until }
	rep, unknown, inCompared, err := comparePass(st, index, cfg, allow, inWindow, true)
	if err != nil {
		return nil, err
	}
	// A kill of unknown event is judged by the sessions beside it, so only once
	// every other mismatch has its verdict. The window limits which rows are
	// judged, not which evidence is read: a neighbour this window did not
	// compare is compared over its whole history, its mismatches never printed.
	vouch := voucher{compared: inCompared, unexplained: unexplainedSessions(rep.mismatches)}
	evidence := map[string]bool{}
	for _, f := range unknown {
		for s := range nearSessions(st, f) {
			if !inCompared[s] && st.sessions[s] != nil {
				evidence[s] = true
			}
		}
	}
	if len(evidence) > 0 {
		full := cfg
		full.since, full.until = st.earliest, st.latest
		ev, _, evCompared, err := comparePass(st, index, full, evidenceAllow, func(s *sSession) bool { return evidence[s.id] }, false)
		if err != nil {
			return nil, err
		}
		for s := range evCompared {
			vouch.compared[s] = true
		}
		for s := range unexplainedSessions(ev.mismatches) {
			if evidence[s] {
				vouch.unexplained[s] = true
			}
		}
	}
	var judged []mismatch
	for _, f := range unknown {
		judged = append(judged, judgeUnknownKill(st, vouch, f))
	}
	allow.apply(judged)
	rep.mismatches = append(rep.mismatches, judged...)
	rep.allowUnused = allow.unused()
	return rep, nil
}

// comparePass compares the sessions pick selects over cfg's window and applies
// allow to every mismatch. It returns the report, the killedUnknown faults in
// the window (left for the caller to judge) and the sessions it compared. A
// judged pass also lists the disk sessions the store never recorded; an
// evidence pass reads only what its sessions need.
func comparePass(st *storeData, index map[string]string, cfg config, allow allowlist, pick func(*sSession) bool, judged bool) (*report, []sFault, map[string]bool, error) {
	rep := &report{cfg: cfg}
	w := newWorld(cfg.since, cfg.until)

	var compared []*sSession
	for _, s := range st.sessions {
		if pick(s) {
			compared = append(compared, s)
		}
	}
	sort.Slice(compared, func(i, j int) bool { return compared[i].id < compared[j].id })
	rep.sessionsCompared = len(compared)
	inCompared := map[string]bool{}
	for _, s := range compared {
		inCompared[s.id] = true
		w.from[s.id] = s.firstTS
	}
	rowsFor := map[string]bool{}
	for _, c := range st.calls {
		rowsFor[c.session] = true
	}
	for _, e := range st.events {
		rowsFor[e.session] = true
	}
	workFor := map[string]bool{} // sessions with a calls or requests row
	for _, c := range st.calls {
		workFor[c.session] = true
	}
	for _, r := range st.requests {
		workFor[r.session] = true
	}
	mainEvents := map[string]map[string]int{} // a session's main-chat event counts
	for _, e := range st.events {
		if e.agent != "" {
			continue
		}
		if mainEvents[e.session] == nil {
			mainEvents[e.session] = map[string]int{}
		}
		mainEvents[e.session][e.event]++
	}
	for _, s := range compared {
		path := ""
		if s.transcriptPath != "" {
			if _, err := os.Stat(s.transcriptPath); err == nil {
				path = s.transcriptPath
			} else if !errors.Is(err, fs.ErrNotExist) {
				return nil, nil, nil, fmt.Errorf("stat transcript of session %s: %w", s.id, err)
			}
		}
		if path == "" {
			path = index[s.id]
		}
		if path == "" {
			if ev := mainEvents[s.id]; !workFor[s.id] && s.model == "" && s.startSource == "startup" && len(ev) == 2 && ev["SessionStart"] == 1 && ev["SessionEnd"] == 1 {
				rep.expect("session-no-prompt", s.id, 0, reasonNoPrompt, promptlessDetail(s, st))
				continue
			}
			// No transcript on disk: whether the session reached the model is
			// unknown, so it is never called promptless.
			rep.add("session-no-transcript", s.id, 0, fmt.Sprintf("work_rows=%t %s", workFor[s.id], promptlessDetail(s, st)))
			continue
		}
		if err := w.readSession(s.id, path); err != nil {
			return nil, nil, nil, err
		}
	}

	// Sessions on disk active in the window that the store never compared.
	other := newWorld(cfg.since, cfg.until)
	other.read = w.read
	var diskIDs []string
	for id := range index {
		diskIDs = append(diskIDs, id)
	}
	sort.Strings(diskIDs)
	for _, id := range diskIDs {
		if !judged || inCompared[id] || st.sessions[id] != nil {
			continue
		}
		info, err := os.Stat(index[id])
		if err != nil {
			return nil, nil, nil, fmt.Errorf("stat transcript %s: %w", index[id], err)
		}
		if info.ModTime().UnixMilli() < cfg.since {
			continue
		}
		if err := other.readSession(id, index[id]); err != nil {
			return nil, nil, nil, err
		}
		t := other.mains[id]
		if t == nil || t.assistants == 0 {
			continue
		}
		detail := fmt.Sprintf("assistant_lines=%d tool_uses=%d subagents=%d", t.assistants, len(t.toolUses), len(other.subagents[id]))
		if rowsFor[id] {
			rep.add("session-row-missing", id, 0, detail)
		} else {
			rep.diskUnrecorded++
			if store, ok := cfg.scratchStores[id]; ok {
				rep.expect("session-unrecorded", id, 0, reasonScratchStore, detail+" scratch_store="+store)
			} else {
				rep.add("session-unrecorded", id, 0, detail)
			}
		}
	}
	rep.transcriptsRead = len(w.read)

	compareSessions(rep, st, w, compared, workFor)
	compareCalls(rep, st, w)
	compareRequests(rep, st, w)
	compareAgents(rep, st, w, inCompared)
	compareParts(rep, st, w)
	compareEvents(rep, st, w, compared)
	killed := matchKilledStopFailures(st)
	stops := matchTerminatedStops(st)
	var unknown []sFault
	for i, f := range st.faults {
		v, isStop := stops[i]
		switch {
		case !w.inWindow(f.ts):
		case killed[i]:
			rep.expect("fault-binary", "", f.ts, reasonKilledStopFailure, "event=StopFailure")
		case isStop && v.session != "" && v.event == "StopFailure":
			rep.expect("fault-terminated", v.session, f.ts, reasonTerminatedFailureNamed, "event=StopFailure")
		case isStop && v.session != "" && f.session != "":
			rep.expect("fault-terminated", v.session, f.ts, reasonTerminatedStopNamed, "event=Stop")
		case isStop && v.session != "":
			rep.expect("fault-terminated", v.session, f.ts, reasonTerminatedStop, "event=Stop")
		case isStop:
			rep.add("fault-terminated", f.session, f.ts, v.detail)
		case lostEnd(st, f):
			rep.expect("fault-"+f.stage, f.session, f.ts, reasonEndKilled, "event=SessionEnd end_reason=lost")
		case f.stage == "binary" && f.session == "" && f.err == killedUnknown:
			unknown = append(unknown, f)
		case f.stage == "transcript" && unfillable(st, w, f) != "":
			rep.expect("fault-transcript-unfillable", f.session, f.ts, reasonUnfillable, unfillable(st, w, f))
		default:
			rep.add("fault-"+f.stage, f.session, f.ts, "", nonEmpty(f.toolUseID)...)
		}
	}

	allow.apply(rep.mismatches)
	return rep, unknown, inCompared, nil
}

// hookLag: every hook runs async, so a transcript entry this close to the
// snapshot's latest row may still have its hook in flight. Its missing row is
// PENDING (`{class}-lag`), never a mismatch; the next snapshot settles it.
const hookLag int64 = 10_000 // ms

func (st *storeData) lagging(ts int64) bool { return ts > st.latest-hookLag }

const (
	reasonNoPrompt               = "a `claude -p` launch given no prompt: Claude Code fires SessionStart and SessionEnd, exits 1 and writes no transcript (no work rows, no other event, no model)"
	reasonUnfillable             = "report-time recovery read every transcript of the session in full and an agent turn or a Stop still had no request to fill, and it recorded that once; this check's own parse of the transcripts confirms they hold none at or after the session's first row (callmeter.RecoverQuiet)"
	reasonKilledStopFailure      = "Claude Code killed the StopFailure hook at a headless exit; the StopFailure row of the session ending right after it (hook or rebuilt from its transcript at SessionEnd) stands for it"
	reasonTerminatedFailureNamed = "the StopFailure hook of the session the fault names ended before it recorded (store busy, or a signal); that session's turn has a StopFailure row (hook or rebuilt from its transcript) no other fault paired"
	reasonTerminatedStopNamed    = "the Stop hook of the session the fault names ended before it recorded (store busy, or a signal); that session's turn has a Stop row (hook or rebuilt from its transcript) no other fault paired"
	reasonTerminatedStop         = "a headless exit cancelled the async Stop hook; the session ending right after it has a Stop row for the turn it cancelled (hook or rebuilt from its transcript), and no session ending there left its turn open"
	reasonKilledUnknown          = "a headless teardown killed a hook before the wrapper read its hook_event_name; every session with a store row within 1 s of the kill's second was compared and holds no unexplained mismatch, so a lost event would surface on one of them"
	reasonScriptBody             = "a part whose program reads its script from a heredoc (python3 - <<EOF) leaves the script unparsed by design: the store keeps no quoted heredoc body and of an unquoted one only its command substitutions (cmdparse.StatusScriptBody)"
	reasonPythonUnparsedPre6     = "a parser before 6 marked a python part unparsed both for a heredoc body the store cut and for code holding an unresolved expansion, by design; the next report run reparses it as script-body or unparsed"
	reasonPythonCodeUnresolved   = "a python -c script or here-string holding an expansion with no known value is unparsed by design: the code the interpreter ran is unknown, so it is never parsed as written (docs/design.md § Parsing a command)"
	reasonNoEndHook              = "Claude Code ended the session without running its SessionEnd hooks: the transcript holds no SessionEnd hook entry, and report-time recovery settled the session and set end_reason never"
	reasonEndKilled              = "the session's SessionEnd hook ran and was lost before it recorded (a headless exit or its timeout killed it, or the wrapper could not run the binary): its own missed.log line named the session, and report-time recovery set end_reason lost"
	reasonStoppedAgent           = "TaskStop stopped the sub-agent while this call ran: Claude Code wrote the call's error result itself and the agent never spoke again, so no PostToolUse, PostToolBatch or SubagentStop fired to write its request, its size or the agent's tool count"
	reasonScratchStore           = "the session ran with CALLMETER_HOME pointing at a scratch store, which records it"
	reasonCommandNotStored       = "the heredoc cutter could not cut this command safely, so the store kept only command_bytes and no report can parse it (docs/design.md § Privacy)"
)

// The tails of the transcript faults report-time recovery records for an agent
// turn or a Stop it read the transcripts in full for and could not fill
// (callmeter.UnfilledAgentTurn, callmeter.UnfilledStopReply; a test pins both).
const (
	unfilledAgentTurn = "no request in its span, and its quiet transcript holds none at or after the session's first hook"
	unfilledStopReply = "no final reply request, and its quiet main transcript holds none at or after the session's first hook"
)

var (
	unfilledAgentFault = regexp.MustCompile(`^agent (\S+) turn stopped (\d+): ` + regexp.QuoteMeta(unfilledAgentTurn) + `$`)
	unfilledStopFault  = regexp.MustCompile(`^prompt (\S+) Stop (\S+): ` + regexp.QuoteMeta(unfilledStopReply) + `$`)
)

// unfillable is the detail of a transcript fault that is recovery's unfillable
// marker and that this check's own parse of the session's transcripts confirms,
// else "". An agent marker holds when the agent's transcript has no request with
// a ts in (the agent's previous stop, the marker's stop] at or after the
// session's first row; a Stop marker when the main transcript has no request
// stamped with its prompt, whose stop_reason is not tool_use, at or after it. A
// transcript this run did not read, or a turn the store does not hold, confirms
// nothing.
func unfillable(st *storeData, w *world, f sFault) string {
	s := st.sessions[f.session]
	if s == nil {
		return ""
	}
	if m := unfilledAgentFault.FindStringSubmatch(f.err); m != nil {
		agent := m[1]
		stopped, err := strconv.ParseInt(m[2], 10, 64)
		if err != nil || w.agents[agent] == nil || w.agents[agent].session != f.session {
			return ""
		}
		var seq int64 = -1
		for _, t := range st.turns[agent] {
			if !t.noStop && t.ts == stopped {
				seq = t.seq
			}
		}
		if seq < 0 {
			return ""
		}
		var previous int64
		for _, t := range st.turns[agent] {
			if !t.noStop && t.seq < seq {
				previous = max(previous, t.ts)
			}
		}
		for _, msg := range w.messages {
			if msg.agent == agent && msg.session == f.session && msg.ts >= s.firstTS && msg.ts > previous && msg.ts <= stopped {
				return ""
			}
		}
		return "agent=" + agent
	}
	if m := unfilledStopFault.FindStringSubmatch(f.err); m != nil {
		prompt := m[1]
		if w.mains[f.session] == nil {
			return ""
		}
		for _, msg := range w.messages {
			if msg.agent == "" && msg.session == f.session && msg.promptID == prompt && msg.ts >= s.firstTS && msg.stopReason != "tool_use" {
				return ""
			}
		}
		return "prompt=" + prompt
	}
	return ""
}

// killedStopFailure is the wrapper's missed.log reason for a StopFailure hook
// killed before its binary ran: no session_id, the ts in whole seconds.
const killedStopFailure = "StopFailure: killed by signal"

// matchKilledStopFailures pairs each killedStopFailure fault, oldest first,
// with one unused main-session StopFailure row at most killRowBefore before
// the fault's second and within it, whose session's last SessionEnd falls in
// [fault, fault+killEndAfter): the kill is that session's headless teardown.
// It returns the matched fault indexes; a fault with no such row stays a
// mismatch.
func matchKilledStopFailures(st *storeData) map[int]bool {
	const killRowBefore, killEndAfter int64 = 10_000, 5_000
	end := map[string]int64{}
	var rows []sEvent
	for _, e := range st.events {
		if e.agent != "" {
			continue
		}
		switch e.event {
		case "SessionEnd":
			end[e.session] = max(end[e.session], e.ts)
		case "StopFailure":
			rows = append(rows, e)
		}
	}
	sort.SliceStable(rows, func(i, j int) bool { return rows[i].ts < rows[j].ts })
	var faults []int
	for i, f := range st.faults {
		if f.stage == "binary" && f.session == "" && f.err == killedStopFailure {
			faults = append(faults, i)
		}
	}
	sort.SliceStable(faults, func(a, b int) bool { return st.faults[faults[a]].ts < st.faults[faults[b]].ts })
	used := make([]bool, len(rows))
	matched := map[int]bool{}
	for _, i := range faults {
		f := st.faults[i]
		for j, e := range rows {
			if used[j] || e.ts < f.ts-killRowBefore || e.ts >= f.ts+1000 {
				continue
			}
			if se := end[e.session]; se < f.ts || se >= f.ts+killEndAfter {
				continue
			}
			used[j], matched[i] = true, true
			break
		}
	}
	return matched
}

// terminatedStop prefixes the binary's fault for a Stop hook ended before it
// recorded (`Stop: terminated by SIGTERM`, `Stop: terminated by store busy`),
// the ts in whole seconds: no session_id when a signal ended it before it read
// its payload, else the session it was recording.
const terminatedStop = "Stop: terminated by "

// terminatedFailure prefixes the binary's fault for a StopFailure hook ended
// before it recorded; only one naming its session is judged (matchTerminatedStops).
const terminatedFailure = "StopFailure: terminated by "

// lostEnd reports whether f is a SessionEnd fault (the binary's `terminated`
// line or the wrapper's `binary` one) naming a session whose latest run
// recovery marked end_reason lost, the fault within that run (at or after the
// second of its latest main-chat SessionStart, as callmeter's endLost reads
// it): the lost hook session-end-killed explains.
func lostEnd(st *storeData, f sFault) bool {
	if f.session == "" || (f.stage != "terminated" && f.stage != "binary") || !strings.HasPrefix(f.err, "SessionEnd: ") {
		return false
	}
	s := st.sessions[f.session]
	if s == nil || s.endReason != endReasonLost {
		return false
	}
	var start int64
	for _, e := range st.events {
		if e.session == f.session && e.agent == "" && e.event == "SessionStart" {
			start = max(start, e.ts)
		}
	}
	return f.ts >= start/1000*1000
}

// stopVerdict is a terminatedStop fault's pairing: the session whose cancelled
// turn its Stop row closes, or empty with the detail naming why none does. A
// fault naming its session is judged against that session alone.
type stopVerdict struct{ session, event, detail string }

// matchTerminatedStops judges each terminatedStop fault, oldest first. The
// sessions its headless exit may have cancelled are the main sessions with a
// SessionEnd in [fault, fault+killEndAfter) and a UserPromptSubmit before the
// fault's second ends; the last such prompt opens the cancelled turn, which a
// Stop or StopFailure row at or after it, before the next prompt, closes. The
// fault pairs with one not-yet-paired session whose turn a Stop row closes
// (hook, or rebuilt from the transcript) only when no such session left its
// turn open: an open turn may be the lost Stop. A fault naming its session
// skips the window: it pairs with that session's turn (its last prompt before
// the fault's second ends) only when a Stop row closes the turn and no other
// fault paired it. A fault paired with nothing stays a mismatch.
func matchTerminatedStops(st *storeData) map[int]stopVerdict {
	const killEndAfter int64 = 5_000
	per := map[string][]sEvent{}
	for _, e := range st.events {
		if e.agent == "" {
			per[e.session] = append(per[e.session], e)
		}
	}
	for _, evs := range per {
		sort.SliceStable(evs, func(i, j int) bool { return evs[i].ts < evs[j].ts })
	}
	sessions := make([]string, 0, len(per))
	for s := range per {
		sessions = append(sessions, s)
	}
	sort.Strings(sessions)
	// turn is the session's turn the fault cancelled: its opening prompt, -1
	// when none came before the fault's second ends, the row closing it (Stop
	// over StopFailure), and whether a StopFailure row is among them.
	turn := func(evs []sEvent, f sFault) (int64, string, bool) {
		prompt := int64(-1)
		for _, e := range evs {
			if e.event == "UserPromptSubmit" && e.ts < f.ts+1000 {
				prompt = e.ts
			}
		}
		closed, failed := "", false
		if prompt < 0 {
			return prompt, closed, failed
		}
		for _, e := range evs {
			if e.ts < prompt {
				continue
			}
			if e.event == "UserPromptSubmit" && e.ts > prompt {
				break
			}
			if e.event == "Stop" || (e.event == "StopFailure" && closed == "") {
				closed = e.event
			}
			failed = failed || e.event == "StopFailure"
		}
		return prompt, closed, failed
	}
	type turnKey struct {
		session, event string
		prompt         int64
	}
	var faults []int
	for i, f := range st.faults {
		if f.stage == "terminated" && (strings.HasPrefix(f.err, terminatedStop) || (f.session != "" && strings.HasPrefix(f.err, terminatedFailure))) {
			faults = append(faults, i)
		}
	}
	sort.SliceStable(faults, func(a, b int) bool { return st.faults[faults[a]].ts < st.faults[faults[b]].ts })
	used := map[string]bool{}
	usedTurn := map[turnKey]bool{}
	prompts := map[string]int64{}
	out := map[int]stopVerdict{}
	for _, i := range faults {
		f := st.faults[i]
		if f.session != "" {
			event := "Stop"
			if strings.HasPrefix(f.err, terminatedFailure) {
				event = "StopFailure"
			}
			evs, stored := per[f.session]
			if _, ok := st.sessions[f.session]; !stored && !ok {
				out[i] = stopVerdict{event: event, detail: "event=" + event + " session_in_store=false"}
				continue
			}
			prompt, closed, failed := turn(evs, f)
			k := turnKey{f.session, event, prompt}
			switch {
			case prompt < 0:
				out[i] = stopVerdict{event: event, detail: "event=" + event + " prompt=none"}
			case closed == "":
				out[i] = stopVerdict{event: event, detail: "event=" + event + " open_turn=" + f.session}
			case (event == "Stop" && closed != "Stop") || (event == "StopFailure" && !failed) || usedTurn[k]:
				out[i] = stopVerdict{event: event, detail: "event=" + event + " unpaired_" + strings.ToLower(event) + "=0"}
			default:
				usedTurn[k] = true
				out[i] = stopVerdict{session: f.session, event: event}
			}
			continue
		}
		var stopped, open []string
		ending := 0
		for _, s := range sessions {
			evs := per[s]
			ended := false
			for _, e := range evs {
				if e.event == "SessionEnd" && e.ts >= f.ts && e.ts < f.ts+killEndAfter {
					ended = true
				}
			}
			prompt, closed, _ := turn(evs, f)
			if !ended || prompt < 0 {
				continue
			}
			ending++
			switch {
			case closed == "":
				open = append(open, s)
			case closed == "Stop" && !used[s] && !usedTurn[turnKey{s, "Stop", prompt}]:
				stopped = append(stopped, s)
				prompts[s] = prompt
			}
		}
		switch {
		case len(open) > 0:
			out[i] = stopVerdict{detail: fmt.Sprintf("event=Stop sessions_ending=%d open_turn=%s", ending, strings.Join(open, ","))}
		case len(stopped) == 0:
			out[i] = stopVerdict{detail: fmt.Sprintf("event=Stop sessions_ending=%d unpaired_stop=0", ending)}
		default:
			used[stopped[0]] = true
			usedTurn[turnKey{stopped[0], "Stop", prompts[stopped[0]]}] = true
			out[i] = stopVerdict{session: stopped[0]}
		}
	}
	return out
}

// killedUnknown is the wrapper's missed.log reason for a hook killed before it
// read hook_event_name: neither the event nor the session is known, the ts in
// whole seconds.
const killedUnknown = "unknown: killed by signal"

// voucher is what the sessions beside a killedUnknown fault can say for it:
// which were compared (in the window, or over their whole history when the
// window did not hold them) and which hold an unexplained mismatch.
type voucher struct{ compared, unexplained map[string]bool }

// unexplainedSessions is every session named by a mismatch no allowlist line or
// classification explained.
func unexplainedSessions(ms []mismatch) map[string]bool {
	out := map[string]bool{}
	for _, m := range ms {
		if m.reason == "" && m.session != "" {
			out[m.session] = true
		}
	}
	return out
}

// nearSessions is every session a killedUnknown fault may have belonged to:
// each with an events, calls or requests row in [fault-1s, fault+2s), its whole
// second widened by 1 s each side.
func nearSessions(st *storeData, f sFault) map[string]bool {
	lo, hi := f.ts-1000, f.ts+2000
	near := map[string]bool{}
	mark := func(session string, ts int64) {
		if session != "" && ts >= lo && ts < hi {
			near[session] = true
		}
	}
	for _, e := range st.events {
		mark(e.session, e.ts)
	}
	for _, c := range st.calls {
		mark(c.session, c.ts)
	}
	for _, r := range st.requests {
		mark(r.session, r.ts)
	}
	return near
}

// judgeUnknownKill judges a killedUnknown fault by its nearSessions. It is
// expected only when there is at least one and each was compared with no
// unexplained mismatch (v carries the allowlist's verdicts), so a lost event
// would surface on that session; otherwise it stays a mismatch naming each
// session that cannot vouch for it.
func judgeUnknownKill(st *storeData, v voucher, f sFault) mismatch {
	near := nearSessions(st, f)
	var cannot []string
	for _, s := range sortedKeys(near) {
		switch {
		case !v.compared[s]:
			cannot = append(cannot, s+":not-compared")
		case v.unexplained[s]:
			cannot = append(cannot, s+":unexplained")
		}
	}
	m := mismatch{class: "fault-binary", ts: f.ts, detail: fmt.Sprintf("event=unknown sessions_near=%d", len(near))}
	switch {
	case len(cannot) > 0:
		m.detail += " unvouched=" + strings.Join(cannot, ",")
	case len(near) > 0:
		m.reason = reasonKilledUnknown
	}
	return m
}

func nonEmpty(ids ...string) []string {
	var out []string
	for _, id := range ids {
		if id != "" {
			out = append(out, id)
		}
	}
	return out
}

// promptlessDetail names what the store holds for a session that never
// reached the model: its model, start source and lifecycle events.
func promptlessDetail(s *sSession, st *storeData) string {
	counts := map[string]int{}
	for _, e := range st.events {
		if e.session == s.id {
			counts[e.event]++
		}
	}
	var evs []string
	for ev, n := range counts {
		evs = append(evs, fmt.Sprintf("%s:%d", ev, n))
	}
	sort.Strings(evs)
	return fmt.Sprintf("model=%s start_source=%s events=%s", orDash(s.model), orDash(s.startSource), orDash(strings.Join(evs, ",")))
}

func compareSessions(rep *report, st *storeData, w *world, compared []*sSession, workFor map[string]bool) {
	for _, s := range compared {
		t := w.mains[s.id]
		if t == nil {
			continue
		}
		// Model activity before the session's first store row: hooks were not
		// yet delivered (a session already running when the plugin was enabled).
		subLines, preAssistants, preTools := 0, t.preAssistants, len(t.preToolUses)
		for _, a := range w.subagents[s.id] {
			subLines += a.windowLines
			preAssistants += a.preAssistants
			preTools += len(a.preToolUses)
		}
		if preAssistants > 0 {
			rep.add("session-pre-first-row", s.id, 0, fmt.Sprintf("first_row=%s assistant_lines=%d tool_uses=%d start_source=%s", msString(s.firstTS), preAssistants, preTools, orDash(s.startSource)))
		}
		// A turn the API refused (model_not_found, a rate limit) leaves a
		// synthetic API error message and no model message: the session reached
		// the model, and compareEvents checks its StopFailure.
		if t.assistants == 0 && subLines == 0 && t.apiErrors == 0 {
			if workFor[s.id] {
				rep.add("session-no-transcript", s.id, 0, fmt.Sprintf("window_lines=%d", t.windowLines))
			} else {
				rep.add("session-promptless", s.id, 0, promptlessDetail(s, st))
			}
			continue
		}
		if s.model == "" && t.assistants > 0 {
			rep.add("session-no-model", s.id, 0, fmt.Sprintf("assistant_lines=%d", t.assistants))
		}
	}
}

func compareCalls(rep *report, st *storeData, w *world) {
	for _, u := range sortedUses(w) {
		c := st.calls[u.id]
		if c == nil {
			rep.add("call-missing", u.session, u.ts, fmt.Sprintf("tool=%s agent=%s ts=%s", u.name, orDash(u.agent), msString(u.ts)), u.id)
			continue
		}
		if c.session != u.session {
			rep.add("call-session", u.session, c.ts, fmt.Sprintf("store_session=%s", orDash(c.session)), u.id)
		}
		if c.agent != u.agent {
			rep.add("call-agent", u.session, c.ts, fmt.Sprintf("transcript_agent=%s store_agent=%s", orDash(u.agent), orDash(c.agent)), u.id)
		} else if u.agent != "" {
			if t := w.agents[u.agent]; t != nil && t.meta.AgentType != "" && c.agentType != t.meta.AgentType {
				rep.add("call-agent-type", u.session, c.ts, fmt.Sprintf("agent=%s transcript_type=%s store_type=%s", u.agent, t.meta.AgentType, orDash(c.agentType)), u.id)
			}
		}
		if c.tool != u.name {
			rep.add("call-tool", u.session, c.ts, fmt.Sprintf("transcript_tool=%s store_tool=%s", u.name, orDash(c.tool)), u.id)
		}
		switch {
		case c.requestID == "" && !w.returned(u.id):
			rep.edge("call-in-flight", u.session, u.ts, fmt.Sprintf("ts=%s", msString(u.ts)), u.id)
		case c.requestID == "":
			if st.lagging(u.ts) {
				rep.edge("call-no-request-lag", u.session, u.ts, "", u.id)
			} else if w.stopped(u.id) {
				rep.expect("call-no-request", u.session, c.ts, reasonStoppedAgent, "agent="+u.agent, u.id)
			} else {
				rep.add("call-no-request", u.session, c.ts, "", u.id)
			}
		case strings.HasPrefix(c.requestID, "pending:"):
			// reported once per request by compareRequests
		case c.requestID != u.msgID:
			rep.add("call-request", u.session, c.ts, fmt.Sprintf("transcript_request=%s store_request=%s", u.msgID, c.requestID), u.id)
		}
		if r, ok := w.results[u.id]; ok && r.ts <= w.until {
			if !c.hasSize && !c.batchOnly {
				if w.stopped(u.id) {
					rep.expect("call-no-size", u.session, c.ts, reasonStoppedAgent, fmt.Sprintf("tool=%s agent=%s", u.name, u.agent), u.id)
				} else {
					rep.add("call-no-size", u.session, c.ts, fmt.Sprintf("tool=%s", u.name), u.id)
				}
			}
			if c.failed >= 0 && (c.failed == 1) != r.isError {
				rep.add("call-failed", u.session, c.ts, fmt.Sprintf("transcript_error=%t store_failed=%d", r.isError, c.failed), u.id)
			}
		}
	}
	var extra []*sCall
	for _, c := range st.calls {
		// A row of a session first recorded before the window had no transcript
		// read, so it is not compared; a row naming no recorded session is.
		if (w.inWindow(c.ts) || c.ts == 0) && w.uses[c.id] == nil && (w.from[c.session] != 0 || st.sessions[c.session] == nil) {
			extra = append(extra, c)
		}
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i].id < extra[j].id })
	for _, c := range extra {
		rep.add("call-extra", c.session, c.ts, fmt.Sprintf("tool=%s agent=%s ts=%d", c.tool, orDash(c.agent), c.ts), c.id)
	}
	// Every hook that writes a call sets its ts: a row without one is a
	// capture gap wherever it falls, so it is not bounded by the window.
	// A call only PostToolBatch wrote is its own class: no PostToolUse or
	// PostToolUseFailure landed, so its real size, duration and outcome (and,
	// in a row older than the batch's ts, its ts) are missing for that one
	// reason. A tool a function-hooks plugin answers in its own tool.call hook
	// fires none of them, every call; its tool rides as an id the allowlist
	// names exactly (`tool={name}`).
	var noTS, batchOnly []*sCall
	for _, c := range st.calls {
		switch {
		case c.batchOnly && (c.noTS || w.inWindow(c.ts)):
			batchOnly = append(batchOnly, c)
		case c.noTS:
			noTS = append(noTS, c)
		}
	}
	sort.Slice(batchOnly, func(i, j int) bool { return batchOnly[i].id < batchOnly[j].id })
	for _, c := range batchOnly {
		ts := "none"
		if !c.noTS {
			ts = msString(c.ts)
		}
		rep.add("call-batch-only", c.session, c.ts, fmt.Sprintf("agent=%s ts=%s", orDash(c.agent), ts), c.id, toolID+c.tool)
	}
	sort.Slice(noTS, func(i, j int) bool { return noTS[i].id < noTS[j].id })
	for _, c := range noTS {
		rep.add("call-no-ts", c.session, 0, fmt.Sprintf("tool=%s agent=%s request=%s", orDash(c.tool), orDash(c.agent), orDash(c.requestID)), c.id)
	}
}

// provisional reports whether a message's calls carry a provisional request
// key: the store holds its batch as a pending request, compared on its own.
func provisional(st *storeData, m *message) bool {
	for id := range m.toolIDs {
		if c := st.calls[id]; c != nil && strings.HasPrefix(c.requestID, "pending:") {
			return true
		}
	}
	return false
}

// fillDue reports whether a pending request's fill came due by --until: a later
// batch of the same chat or sub-agent recorded (a call holding a request key:
// one still running fills nothing yet), its Stop or SubagentStop, or the
// session's end (docs/design.md § Sources: a request not yet on disk is filled
// at the next batch of the same chat or agent, or at the next Stop or
// SubagentStop). Before any of them it is still open, not lost.
func fillDue(st *storeData, r *sRequest, until int64) bool {
	for _, c := range st.calls {
		if c.session == r.session && c.agent == r.agent && c.requestID != "" && c.requestID != r.id && c.ts > r.ts && c.ts <= until {
			return true
		}
	}
	for _, e := range st.events {
		if e.session != r.session || e.ts <= r.ts || e.ts > until {
			continue
		}
		if e.event == "SessionEnd" || (e.event == "Stop" && r.agent == "") || (e.event == "SubagentStop" && e.agent == r.agent) {
			return true
		}
	}
	return false
}

// turnEnded reports whether the turn holding a text-only message ended by
// --until: its chat's Stop, its sub-agent's SubagentStop, or the session's end.
// The binary writes a text-only reply's request only there (sweepRequests), so
// before any of them the message is open, not untracked.
func turnEnded(st *storeData, m *message, until int64) bool {
	for _, e := range st.events {
		if e.session != m.session || e.ts < m.ts || e.ts > until {
			continue
		}
		if e.event == "SessionEnd" || (e.event == "Stop" && m.agent == "") || (e.event == "SubagentStop" && e.agent == m.agent) {
			return true
		}
	}
	return false
}

// returned reports whether any of the calls got its tool_result by --until.
// A request row is written at PostToolBatch, once its calls return: a batch
// still running at the snapshot has no request row yet, and is in flight
// rather than missing.
func (w *world) returned(ids ...string) bool {
	for _, id := range ids {
		if r, ok := w.results[id]; ok && r.ts <= w.until {
			return true
		}
	}
	return false
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func sortedUses(w *world) []*use {
	out := make([]*use, 0, len(w.uses))
	for _, u := range w.uses {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

func compareRequests(rep *report, st *storeData, w *world) {
	var ids []string
	for id := range w.messages {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	// sumTS is the latest row behind a token-sum disagreement: the sums differ
	// only through a request-missing or request-tokens mismatch.
	var sumTS int64
	for _, id := range ids {
		m := w.messages[id]
		r := st.requests[id]
		if len(m.toolIDs) == 0 {
			if r != nil {
				continue
			}
			if !turnEnded(st, m, w.until) {
				rep.edge("request-untracked-open", m.session, m.ts, fmt.Sprintf("agent=%s", orDash(m.agent)), m.id)
				continue
			}
			if st.lagging(m.ts) {
				rep.edge("request-untracked-lag", m.session, m.ts, fmt.Sprintf("agent=%s", orDash(m.agent)), m.id)
				continue
			}
			// One line per message, so its own ts scopes it.
			rep.add("request-untracked", m.session, m.ts, fmt.Sprintf("agent=%s %s", orDash(m.agent), m.usage), m.id)
			rep.untrackedMessages++
			rep.untrackedTokens.addTo(m.usage)
			continue
		}
		if r == nil && provisional(st, m) {
			continue // its batch holds a provisional key: reported once below
		}
		if r == nil && !w.returned(sortedKeys(m.toolIDs)...) {
			rep.edge("request-in-flight", m.session, m.ts, fmt.Sprintf("agent=%s tool_uses=%d ts=%s", orDash(m.agent), len(m.toolIDs), msString(m.ts)), m.id)
			continue
		}
		if r == nil && st.lagging(m.ts) {
			rep.edge("request-missing-lag", m.session, m.ts, fmt.Sprintf("agent=%s tool_uses=%d", orDash(m.agent), len(m.toolIDs)), m.id)
			continue
		}
		// A stopped sub-agent's request has no row and no hook left to write
		// one: it stays out of both sums, like one pending at --until.
		if r == nil && w.stopped(sortedKeys(m.toolIDs)...) {
			rep.expect("request-missing", m.session, m.ts, reasonStoppedAgent, fmt.Sprintf("agent=%s tool_uses=%d ts=%s", orDash(m.agent), len(m.toolIDs), msString(m.ts)), m.id)
			continue
		}
		// Pending at --until stays out of both sums; a missing row counts on
		// the transcript's side alone.
		rep.tokenTranscript.addTo(m.usage)
		if r == nil {
			sumTS = max(sumTS, m.ts)
			rep.add("request-missing", m.session, m.ts, fmt.Sprintf("agent=%s tool_uses=%d ts=%s", orDash(m.agent), len(m.toolIDs), msString(m.ts)), m.id)
			continue
		}
		rep.tokenStore.addTo(r.usage)
		if r.usage != m.usage {
			sumTS = max(sumTS, r.ts)
			rep.add("request-tokens", m.session, r.ts, fmt.Sprintf("transcript{%s} store{%s}", m.usage, r.usage), m.id)
		}
		if r.session != m.session || r.agent != m.agent {
			rep.add("request-owner", m.session, r.ts, fmt.Sprintf("transcript=%s/%s store=%s/%s", m.session, orDash(m.agent), orDash(r.session), orDash(r.agent)), m.id)
		}
		if r.model != m.model {
			rep.add("request-model", m.session, r.ts, fmt.Sprintf("transcript=%s store=%s", m.model, orDash(r.model)), m.id)
		}
		if m.stopReason != "" && r.stopReason != m.stopReason {
			rep.add("request-stop-reason", m.session, r.ts, fmt.Sprintf("transcript=%s store=%s", m.stopReason, orDash(r.stopReason)), m.id)
		}
		if r.calls != int64(len(m.toolIDs)) {
			rep.add("request-calls", m.session, r.ts, fmt.Sprintf("transcript=%d store=%d", len(m.toolIDs), r.calls), m.id)
		}
	}
	if rep.tokenTranscript != rep.tokenStore {
		rep.add("token-sum", "", sumTS, fmt.Sprintf("transcript{%s} store{%s}", rep.tokenTranscript, rep.tokenStore))
	}
	var reqIDs []string
	for id := range st.requests {
		reqIDs = append(reqIDs, id)
	}
	sort.Strings(reqIDs)
	for _, id := range reqIDs {
		r := st.requests[id]
		if r.pending != 0 || strings.HasPrefix(id, "pending:") {
			detail := fmt.Sprintf("agent=%s ts=%s", orDash(r.agent), msString(r.ts))
			if fillDue(st, r, w.until) {
				rep.add("request-pending", r.session, r.ts, detail, id)
			} else {
				rep.edge("request-pending-open", r.session, r.ts, detail, id)
			}
			continue
		}
		if w.inWindow(r.ts) && w.messages[id] == nil && (w.from[r.session] != 0 || st.sessions[r.session] == nil) {
			rep.add("request-extra", r.session, r.ts, fmt.Sprintf("agent=%s", orDash(r.agent)), id)
		}
	}
}

func compareAgents(rep *report, st *storeData, w *world, inCompared map[string]bool) {
	var ids []string
	for id := range w.agents {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		t := w.agents[id]
		if t.windowLines == 0 {
			continue
		}
		a := st.agents[id]
		if a == nil {
			rep.add("agent-missing", t.session, 0, fmt.Sprintf("type=%s tool_uses=%d", orDash(t.meta.AgentType), len(t.toolUses)), id)
			continue
		}
		if a.session != t.session {
			rep.add("agent-session", t.session, 0, fmt.Sprintf("store_session=%s", orDash(a.session)), id)
		}
		if t.meta.AgentType != "" && a.agentType != t.meta.AgentType {
			rep.add("agent-type", t.session, 0, fmt.Sprintf("transcript=%s store=%s", t.meta.AgentType, orDash(a.agentType)), id)
		}
		if t.meta.ToolUseID != "" && a.parent != t.meta.ToolUseID {
			rep.add("agent-parent", t.session, 0, fmt.Sprintf("transcript=%s store=%s", t.meta.ToolUseID, orDash(a.parent)), id)
		}
		if a.toolUses >= 0 && a.toolUses != int64(len(t.toolUses)) {
			detail := fmt.Sprintf("transcript=%d store=%d", len(t.toolUses), a.toolUses)
			// The count is written at SubagentStop: a stopped agent's open last
			// turn never gets one, so its stopped calls are the whole gap.
			var cut int64
			for u := range t.toolUses {
				if w.stopped(u) {
					cut++
				}
			}
			if turns := st.turns[id]; cut > 0 && int64(len(t.toolUses))-a.toolUses == cut && len(turns) > 0 && lastTurn(turns).noStop {
				rep.expect("agent-tool-uses", t.session, 0, reasonStoppedAgent, detail, id)
			} else if len(turns) > 0 && a.toolUses < int64(len(t.toolUses)) && turnOpen(st, t.session, lastTurn(turns)) && t.lastTS > st.latest-quietAfterMS {
				// Not due: the count lands at the SubagentStop of the turn
				// still running, as recovery waits an hour for a quiet one.
				rep.edge("agent-tool-uses-open", t.session, 0, detail, id)
			} else {
				rep.add("agent-tool-uses", t.session, 0, detail, id)
			}
		}
		// Every turn ends in an end_turn message; a last turn cut short ends in none.
		implied := len(t.endTurns)
		if t.assistants > 0 && t.lastStop != "end_turn" {
			implied++
		}
		turns := st.turns[id]
		if t.assistants > 0 && len(turns) != implied {
			rep.add("agent-turns", t.session, 0, fmt.Sprintf("transcript_turns=%d store_turns=%d", implied, len(turns)), id)
		}
		for _, turn := range turns {
			if turn.noStart {
				rep.add("agent-turn-no-start", t.session, turn.ts, fmt.Sprintf("seq=%d", turn.seq), id)
			}
			if turn.noStop && t.lastStop == "end_turn" && t.lastTS < w.until-settle {
				rep.add("agent-turn-open", t.session, turn.ts, fmt.Sprintf("seq=%d", turn.seq), id)
			}
		}
	}
	var stored []string
	for id, a := range st.agents {
		if inCompared[a.session] && w.agents[id] == nil {
			stored = append(stored, id)
		}
	}
	sort.Strings(stored)
	for _, id := range stored {
		rep.add("agent-extra", st.agents[id].session, st.agents[id].started, fmt.Sprintf("type=%s", orDash(st.agents[id].agentType)), id)
	}
}

// compareParts checks every Bash call's command_parts. The parse runs at
// report time only (docs/design.md § Parsing a command), so a call newer than
// every parsed call waits for the next report run: parts-pending-report. A call
// older than a parsed one, still without parts, was skipped: parts-missing.
func compareParts(rep *report, st *storeData, w *world) {
	var lastParsed int64
	for id := range st.parts {
		if c := st.calls[id]; c != nil {
			lastParsed = max(lastParsed, c.ts)
		}
	}
	for _, u := range sortedUses(w) {
		c := st.calls[u.id]
		if u.name != "Bash" || c == nil {
			continue
		}
		parts := st.parts[u.id]
		if len(parts) == 0 && c.noCommand {
			rep.expect("parts-command-not-stored", u.session, c.ts, reasonCommandNotStored, "", u.id)
			continue
		}
		if len(parts) == 0 {
			if c.ts > lastParsed {
				rep.edge("parts-pending-report", u.session, c.ts, fmt.Sprintf("last_parsed_call=%s", msString(lastParsed)), u.id)
			} else {
				rep.add("parts-missing", u.session, c.ts, fmt.Sprintf("ts=%s last_parsed_call=%s", msString(c.ts), msString(lastParsed)), u.id)
			}
			continue
		}
		counts := map[string]int{}
		stale := false
		for _, p := range parts {
			// A parser before scriptBodyParser marked a python part unparsed
			// both for a heredoc body the store cut and for code holding an
			// unresolved expansion; from it on the heredoc case is script-body
			// and an unparsed python part is a -c or <<< code holding an
			// unresolved expansion alone (cmdparse.go, errCodeUnresolved).
			switch {
			case p.status == cmdparse.StatusUnparsed && p.lang == cmdparse.LangPython && p.parser < scriptBodyParser:
				counts[statusPythonUnparsedPre6]++
			case p.status == cmdparse.StatusUnparsed && p.lang == cmdparse.LangPython:
				counts[statusPythonCodeUnresolved]++
			default:
				counts[p.status]++
			}
			if p.parser < st.maxParser {
				stale = true
			}
		}
		for status, n := range counts {
			switch {
			case status == cmdparse.StatusScriptBody:
				rep.expect("parse-script-body", u.session, c.ts, reasonScriptBody, fmt.Sprintf("parts=%d of %d", n, len(parts)), u.id)
			case status == statusPythonCodeUnresolved:
				rep.expect("parse-python-code-unresolved", u.session, c.ts, reasonPythonCodeUnresolved, fmt.Sprintf("parts=%d of %d", n, len(parts)), u.id)
			case status == statusPythonUnparsedPre6:
				rep.expect("parse-python-unparsed-pre6", u.session, c.ts, reasonPythonUnparsedPre6, fmt.Sprintf("parts=%d of %d", n, len(parts)), u.id)
			case status != "ok":
				rep.add("parse-"+orDash(status), u.session, c.ts, fmt.Sprintf("parts=%d of %d", n, len(parts)), u.id)
			}
		}
		if stale {
			rep.add("parts-stale", u.session, c.ts, fmt.Sprintf("store_max_parser=%d", st.maxParser), u.id)
		}
	}
}

// compareEvents checks the lifecycle events the transcripts imply, by count
// over each session's window: a session with activity has a SessionStart;
// every compaction boundary, in the main transcript or a sub-agent's, a
// PostCompact and a SessionStart(compact), and every PreCompact a PostCompact;
// every finished interactive turn (turn_duration; a headless run writes none)
// a Stop with stop_hook_active false (a Stop hook that blocks the turn's end
// makes Claude Code fire Stop again in the same turn, stop_hook_active true);
// every API error message a StopFailure; every model-bound prompt a
// UserPromptSubmit carrying its promptId, repeated once per queued command.
func compareEvents(rep *report, st *storeData, w *world, compared []*sSession) {
	type tally struct {
		n       map[string]int
		prompts map[string]bool
	}
	per := map[string]*tally{}
	for _, e := range st.events {
		if !w.inWindow(e.ts) {
			continue
		}
		t := per[e.session]
		if t == nil {
			t = &tally{n: map[string]int{}, prompts: map[string]bool{}}
			per[e.session] = t
		}
		if e.agent != "" {
			t.n["agent:"+e.event]++
			continue
		}
		if e.event == "Stop" && e.stopHookActive {
			t.n["Stop:hook-active"]++
			continue
		}
		if e.event == "Stop" && st.lagging(e.ts) {
			t.n["Stop:lag"]++
		}
		t.n[e.event]++
		if e.event == "SessionStart" && e.source == "compact" {
			t.n["SessionStart:compact"]++
		}
		if e.event == "UserPromptSubmit" && e.promptID != "" {
			t.prompts[e.promptID] = true
		}
	}
	// A session's latest run ended when its main chat holds a SessionEnd at or
	// after its latest SessionStart, over the whole history, not the window.
	latestStart, lastEnd := map[string]int64{}, map[string]int64{}
	for _, e := range st.events {
		if e.agent != "" {
			continue
		}
		switch e.event {
		case "SessionStart":
			latestStart[e.session] = max(latestStart[e.session], e.ts)
		case "SessionEnd":
			lastEnd[e.session] = max(lastEnd[e.session], e.ts)
		}
	}
	for _, s := range compared {
		t := w.mains[s.id]
		if t == nil || t.windowLines == 0 {
			continue
		}
		c := per[s.id]
		if c == nil {
			c = &tally{n: map[string]int{}, prompts: map[string]bool{}}
		}
		if end, ok := lastEnd[s.id]; !ok || end < latestStart[s.id] {
			switch {
			case s.endReason == endReasonLost:
				// The store recorded the loss from the hook's own line.
				rep.expect("session-end-killed", s.id, 0, reasonEndKilled, fmt.Sprintf("transcript_end_hook=%t end_reason=lost", t.endHook))
			case t.endHook:
				// The hook ran and the store lost it.
				rep.add("session-no-end-hook", s.id, 0, "transcript_end_hook=true end_reason="+orDash(s.endReason))
			case s.endReason == endReasonNever:
				rep.expect("session-no-end-hook", s.id, 0, reasonNoEndHook, "end_reason=never")
			case s.lastTS > st.latest-quietAfterMS:
				// Not due: recovery marks a session only after an hour quiet.
				rep.edge("session-no-end-hook-live", s.id, 0, "end_reason="+orDash(s.endReason))
			case w.writes[s.id] > st.latest-quietAfterMS:
				// Not due either: recovery also counts a transcript file written
				// within the hour as live (callmeter.RecoverQuiet), whatever the
				// store's last row.
				rep.edge("session-no-end-hook-live", s.id, 0, "end_reason="+orDash(s.endReason)+" transcript_written="+msString(w.writes[s.id]))
			default:
				rep.add("session-no-end-hook", s.id, 0, "end_reason="+orDash(s.endReason)+" quiet past QuietAfter and unmarked: `callmeter report` on the snapshot settles it")
			}
		}
		if c.n["SessionStart"] == 0 && t.assistants > 0 && t.preAssistants == 0 {
			rep.add("event-no-sessionstart", s.id, 0, fmt.Sprintf("session_end=%d stop=%d", c.n["SessionEnd"], c.n["Stop"]))
		}
		subCompacts := 0
		for _, a := range w.subagents[s.id] {
			subCompacts += a.compacts
		}
		all := t.compacts + subCompacts
		for _, ev := range []string{"PostCompact", "SessionStart:compact"} {
			if c.n[ev]+c.n["agent:"+ev] != all {
				rep.add("event-compact", s.id, 0, fmt.Sprintf("%s transcript_boundaries=%d (main %d, sub-agents %d) store=%d", ev, all, t.compacts, subCompacts, c.n[ev]+c.n["agent:"+ev]))
			}
		}
		if subCompacts > 0 && c.n["agent:PostCompact"] < subCompacts {
			rep.add("event-compact-subagent", s.id, 0, fmt.Sprintf("sub_agent_boundaries=%d store_agent_compactions=%d", subCompacts, c.n["agent:PostCompact"]))
		}
		if pre, post := c.n["PreCompact"]+c.n["agent:PreCompact"], c.n["PostCompact"]+c.n["agent:PostCompact"]; pre != post {
			rep.add("event-precompact-unfinished", s.id, 0, fmt.Sprintf("pre=%d post=%d", pre, post))
		}
		if t.entrypoints["cli"] && len(t.entrypoints) == 1 && c.n["Stop"] != t.turnDurations {
			detail := fmt.Sprintf("transcript_turns=%d store_stop=%d", t.turnDurations, c.n["Stop"])
			// Stops within hookLag of the snapshot whose turn_duration line
			// falls after --until: the turn's end is not yet on both sides.
			if excess := c.n["Stop"] - t.turnDurations; excess > 0 && excess <= c.n["Stop:lag"] {
				rep.edge("event-stop-lag", s.id, 0, detail)
			} else {
				rep.add("event-stop", s.id, 0, detail)
			}
		}
		if c.n["StopFailure"] != t.apiErrors {
			rep.add("event-stopfailure", s.id, 0, fmt.Sprintf("transcript_api_errors=%d store=%d", t.apiErrors, c.n["StopFailure"]))
		}
		var missing, extra []string
		for p := range t.prompts {
			if !c.prompts[p] {
				missing = append(missing, p)
			}
		}
		for p := range c.prompts {
			if !t.prompts[p] {
				extra = append(extra, p)
			}
		}
		sort.Strings(missing)
		sort.Strings(extra)
		if len(missing) > 0 {
			rep.add("event-prompt-missing", s.id, 0, fmt.Sprintf("transcript_prompts=%d", len(t.prompts)), missing...)
		}
		if len(extra) > 0 {
			rep.add("event-prompt-extra", s.id, 0, fmt.Sprintf("store_prompts=%d", len(c.prompts)), extra...)
		}
		if repeats := c.n["UserPromptSubmit"] - len(c.prompts); repeats != t.queued {
			rep.add("event-prompt-repeat", s.id, 0, fmt.Sprintf("store_repeats=%d transcript_queued=%d", repeats, t.queued))
		}
	}
}

// allowlist entries: `class | scopes | reason`, scopes space-separated, any one
// matching: a prefix of a session, call, request, prompt or agent id;
// `tool={name}`, matched exactly, for a class whose mismatch carries its tool;
// or `ts<{RFC 3339}`, a mismatch whose row is older than that time (a mismatch
// with no row time never matches it). No entry matches a whole class.
type allowEntry struct {
	class  string
	ids    []string
	before int64 // a `ts<` scope's bound (unix ms); 0: none
	reason string
	line   int // its line in the file
	hits   int // the mismatches it explained
}

type allowlist []allowEntry

// toolID prefixes the tool id a call-batch-only mismatch carries.
const toolID = "tool="

// tsBefore prefixes a scope naming the rows older than a time.
const tsBefore = "ts<"

func loadAllowlist(path string) (allowlist, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read allowlist %s: %w", path, err)
	}
	var out allowlist
	for n, l := range strings.Split(string(raw), "\n") {
		l = strings.TrimSpace(l)
		if l == "" || strings.HasPrefix(l, "#") {
			continue
		}
		f := strings.SplitN(l, "|", 3)
		if len(f) != 3 || strings.TrimSpace(f[0]) == "" || strings.TrimSpace(f[1]) == "" || strings.TrimSpace(f[2]) == "" {
			return nil, fmt.Errorf("allowlist %s line %d: want `class | ids | reason`, each non-empty", filepath.Base(path), n+1)
		}
		e := allowEntry{class: strings.TrimSpace(f[0]), reason: strings.TrimSpace(f[2]), line: n + 1}
		for _, id := range strings.Fields(f[1]) {
			switch {
			case id == "*":
				return nil, fmt.Errorf("allowlist %s line %d: %s: `*` is refused; scope it by id, `tool=` or `ts<`", filepath.Base(path), n+1, e.class)
			case strings.HasPrefix(id, tsBefore):
				if e.before != 0 {
					return nil, fmt.Errorf("allowlist %s line %d: %s: one `ts<` scope per entry", filepath.Base(path), n+1, e.class)
				}
				t, err := time.Parse(time.RFC3339, strings.TrimPrefix(id, tsBefore))
				if err != nil {
					return nil, fmt.Errorf("allowlist %s line %d: %s: %q: %w", filepath.Base(path), n+1, e.class, id, err)
				}
				e.before = t.UnixMilli()
			default:
				e.ids = append(e.ids, id)
			}
		}
		out = append(out, e)
	}
	return out, nil
}

func (a allowlist) apply(ms []mismatch) {
	for i := range ms {
		for j := range a {
			if a[j].class == ms[i].class && a[j].matches(ms[i]) {
				ms[i].reason = a[j].reason
				a[j].hits++
				break
			}
		}
	}
}

// unused is the line of every entry no mismatch matched: a line explaining
// nothing in this window.
func (a allowlist) unused() []int {
	var out []int
	for _, e := range a {
		if e.hits == 0 {
			out = append(out, e.line)
		}
	}
	return out
}

func (e allowEntry) matches(m mismatch) bool {
	if e.before != 0 && m.ts > 0 && m.ts < e.before {
		return true
	}
	for _, id := range e.ids {
		if strings.HasPrefix(id, toolID) {
			// A tool name matches exactly: a prefix would explain a sibling tool.
			if slices.Contains(m.ids, id) {
				return true
			}
			continue
		}
		if m.session != "" && strings.HasPrefix(m.session, id) {
			return true
		}
		for _, mid := range m.ids {
			if strings.HasPrefix(mid, id) {
				return true
			}
		}
	}
	return false
}

// stopped reports whether TaskStop cut any of the calls: a sub-agent's call
// whose error result Claude Code wrote within stopWrite after a TaskStop naming
// that agent, with no assistant line of the agent after it.
func (w *world) stopped(ids ...string) bool {
	for _, id := range ids {
		u := w.uses[id]
		r, done := w.results[id]
		if u == nil || u.agent == "" || !done || !r.isError {
			continue
		}
		t := w.agents[u.agent]
		if t == nil || t.lastAssistant >= r.ts {
			continue
		}
		for _, stop := range w.taskStops[u.agent] {
			if r.ts >= stop && r.ts-stop <= stopWrite {
				return true
			}
		}
	}
	return false
}

// lastTurn is the turn of highest seq.
func lastTurn(turns []sTurn) sTurn {
	last := turns[0]
	for _, t := range turns[1:] {
		if t.seq > last.seq {
			last = t
		}
	}
	return last
}

// turnOpen reports whether a sub-agent turn has no SubagentStop and no
// SessionEnd of its session at or after its start: still running.
func turnOpen(st *storeData, session string, turn sTurn) bool {
	if !turn.noStop {
		return false
	}
	for _, e := range st.events {
		if e.session == session && e.event == "SessionEnd" && e.ts >= turn.ts {
			return false
		}
	}
	return true
}
