# Context management

Status: **proposed — not implemented.** This document describes behaviour hal
does not have yet. Until it is implemented, `design/features/basic-functions`
remains an accurate description of the program, including its statement that
context-window tracking is absent.

This item covers two related capabilities:

1. **A context indicator** — how much of the model's context window the current
   conversation occupies, shown in the interface.
2. **Compaction** — replacing older parts of the conversation with a summary so a
   long session can continue inside the window, either automatically when the
   window is nearly full or on an explicit command.

Both belong to one feature because they answer one question — "how much room is
left, and what happens when it runs out" — and because the indicator is what
tells the user when compaction happened and what it saved. How the two are
implemented (measurement, model-limit discovery, the stored format of a
compaction) belongs to a technical document, which does not exist yet; see §8.

## 1. Purpose

A long conversation eventually exceeds what the model accepts. Today hal has no
notion of that limit: the footer reports cumulative token totals, which say how
much has been spent, not how full the window is, and the first sign of trouble is
a provider error rejecting the request. The user cannot plan around a limit they
cannot see, and cannot recover from one except by starting over.

The feature gives the user:

- an always-visible `used / limit` figure for the current conversation, distinct
  from the cumulative spend counters;
- a warning as the window fills;
- automatic compaction before a request would be rejected, so a long session
  keeps working;
- a manual command to compact on demand, with optional guidance about what to
  preserve;
- a record of what compaction did, so the conversation's losses are visible
  rather than silent.

## 2. Vocabulary

- **Context window** — the maximum number of tokens the model accepts in one
  request, including the system prompt, the tool definitions, and every message.
- **Context** — what the *next* request will send: system prompt + tool
  definitions + the active conversation.
- **Used** — the current size of the context in tokens.
- **Limit** — the context window of the active model.
- **Compaction** — replacing the oldest part of the active conversation with a
  summary, so that used drops while the thread of work is preserved.
- **Active context** — the messages the model sees. It can be smaller than the
  stored log; see §6.4.
- **Turn** and **run** — as defined in `design/features/basic-functions` §2.1.

## 3. The context indicator

### 3.1 What is displayed

The indicator is a single figure: **used / limit**, with the percentage of the
window it represents.

- **Terminal UI.** The footer gains a context segment beside the model, status,
  cumulative `↑`/`↓` counts, and turn count:

  ```
  ~/work/hal        gpt-4o-mini · ready · ctx ~12.4k/128k 10% · ↑120k ↓8.2k · 3 turns
  ```

  The cumulative `↑`/`↓` counters stay as they are; they answer a different
  question (how much has been spent), and removing them would be a regression.
- **Web console.** The footer's right-hand side gains the same `ctx used/limit
  percent` segment, in the same order relative to the other segments.
- **No measurement yet.** Before the first response of a conversation, the figure
  is an estimate (§3.4) and is marked with `~`:
  `ctx ~12.4k/128k 10%`.

The indicator is a persistent interface element, so it exists where there is a
persistent interface: the terminal footer and the web footer. One-shot print and
JSON runs have no footer; JSON mode reports the same figures on its event stream
(§4.6), and print mode stays quiet about them except when compaction happens.
Compaction itself is a property of the run, not of the interface, so it works in
all four modes.

The numbers are abbreviated the way the existing counters are (`1.2k`, `3.4M`).

### 3.2 Severity

The indicator changes appearance as the window fills, so it reads at a glance:

| Occupancy | State | Presentation |
|---|---|---|
| below 70% | normal | default footer colour |
| 70% to the auto-compaction threshold | warning | warning colour |
| at or above the auto-compaction threshold | critical | error colour, and the status shows `compacting…` while compaction runs |
| above 100% | over | error colour; `over by N` |

The thresholds use the configured auto-compaction threshold (§5) so that
"critical" always means "compaction is about to happen or has just happened",
rather than a second set of magic numbers.

### 3.3 Where the limit comes from

Resolution is per setting, highest priority first:

1. **Explicit configuration** — `--context-window <tokens>` or `context_window`
   in the config file. Always wins, and is never second-guessed.
2. **The endpoint's model metadata** — when the OpenAI-compatible service
   publishes a context limit for the selected model, hal uses it. This is
   best-effort: many services publish nothing, and a service that cannot be
   reached for metadata is not an error — resolution simply continues to (3).
3. **A built-in table** for common model families, matched by model id prefix
   (`gpt-4o`, `o1`, `o3`, `llama3`, …). The table is small and conservative: it
   exists so the common cases work without configuration, not to become a vendor
   matrix.
4. **A default of 128k**, marked as assumed.

The limit is therefore always known, which keeps the indicator a single shape.
Whenever the value comes from (3) or (4), `/context` says so explicitly (`from
the built-in table`, `assumed`); the footer shows only the figure, since a
caveat on every line would crowd it. A wrong limit only changes *when* compaction
happens; it never corrupts a conversation.

The limit follows the model: `/model` re-resolves it immediately, and switching
to a model with a smaller window re-evaluates the threshold (§4.4, §6.3). A
resumed session whose recorded model differs from the current one uses the
current model.

### 3.4 How "used" is measured

The displayed number is the size of the next request, and it is as exact as the
provider allows:

- **Measured.** After a response, the provider reports the exact input and output
  token counts for that request. Those two cover the entire active context at
  that moment, so `used` is their sum until something is appended.
- **Estimated.** Anything appended after that response — tool results, a new
  user message, and the state after a compaction — is estimated from its text,
  and the estimate is marked `~` until the next response replaces it with a
  measured figure.
- **First request.** Nothing has been measured yet, so the whole context is
  estimated: system prompt, tool definitions, and the messages.

Estimation must be reasonable for non-ASCII text as well, so a conversation in
Chinese or Japanese is not reported at a fraction of its real size. The estimate
is allowed to be approximate; it drives a warning and the compaction decision,
and the exact figure arrives one turn later. How the estimate is computed belongs
to the technical design.

What counts toward `used`:

- the system prompt, including any context files appended to it;
- the definitions of the enabled tools (they are sent with every request);
- every message in the active context, including tool results;
- the active summary after a compaction (§4.1).

`/clear` does not change it (it clears the visible transcript only, as it does
today). `/new` resets it to the size of the system prompt and tool definitions.
Switching or resuming a session recomputes it from the restored context (§6.1).

### 3.5 The `/context` command

`/context` prints a breakdown, so the figure in the footer is explainable:

```
context
  model         gpt-4o-mini
  window        128k (from the built-in table)
  used          12,431 tokens (10%)
    system       1,204
    tools          812
    messages    10,415
      summary     1,832 (18 earlier messages)
      recent      8,583 (6 messages, 3 turns)
  headroom      115,569
  auto-compact  on, at 80% (102k)
  last request  12,398 input + 214 output (measured)
```

The breakdown is what makes the footer figure debuggable: a user who thinks the
count is wrong can see which part is large, and whether the number is measured or
estimated.

It is available in the terminal UI and the web console. `/context summary` prints
the text of the active summary, if there is one, so the user can see exactly what
the model was told about the compacted part of the conversation. `/context` and
`/context summary` are commands of the two consoles, like `/session` and
`/switch`; they have no equivalent in print or JSON mode, where the same figures
reach the caller on the event stream instead.

The breakdown's categories are the ones the context is assembled from, not a new
classification: system prompt (with context files), tool definitions, the summary
(if any), and the messages after it. The per-category numbers are estimates; the
`used` line is the same figure the footer shows, measured when the provider has
reported one and estimated otherwise, so the categories can be a little off from
it. The `last request` line is what the provider actually reported.

## 4. Compaction

### 4.1 What compaction does

Compaction takes the oldest part of the active conversation, asks the model to
summarize it, and replaces it in the active context with:

```
[summary of the conversation so far]
<the summary>
[the most recent turns follow]
```

The summary is produced by the same model, in a separate request that is not part
of the conversation, and is inserted as one clearly labelled message. It takes
the role of a user message, so the conversation it produces is always valid: the
summary can never leave a tool call without its result, or a result without its
call. The system prompt and the tool definitions are never compacted: they are
the instructions, not the conversation.

The summarization request carries no tool definitions, so the model can only
answer it with text — a summary cannot turn into a tool call, whatever the
conversation it is summarizing contains.

### 4.2 What is kept verbatim

- The system prompt and any context files in it.
- The definitions of the enabled tools.
- The most recent turns, verbatim: the last 3 user turns with their assistant
  messages and tool results. Three turns is the default and is not configurable
  in this design; keeping the recent part exact is what makes the conversation
  continue coherently, and a configurable tail would add a setting whose right
  value is not obvious.
- Within that tail, a tool result may still be shortened to an `output omitted`
  marker when the target size cannot otherwise be met (§4.4). This is the one
  exception to "verbatim", and the compaction notification says when it happened.

Nothing newer than the summary is lost. Nothing older than the kept tail survives
except through the summary.

Compaction never splits a unit: an assistant message that requested tools is
always kept or summarized together with the tool results that answered it. A cut
point is only chosen at a message boundary where the conversation is consistent
(request, results, next request).

### 4.3 What the summary must contain

The summarization request instructs the model to produce a compact, factual
summary that would let it continue the work without the original messages,
covering:

- what the user asked for, including constraints and preferences;
- decisions taken and the reasons for them;
- files read, created, and modified, with their paths;
- commands run and what they showed, including failures and how they were
  resolved;
- errors encountered and fixes applied;
- the current state of the work, and what remains to be done;
- anything the user asked to remember or to avoid.

The summary is explicitly labelled as a summary of earlier conversation, so the
model does not mistake it for a user instruction or for current state. It must
not invent progress that did not happen; a summary that claims work is finished
when it is not is the main risk of this feature, and the prompt states that the
summary is a record, not a plan.

Repeated compaction is allowed: compacting a context that already contains a
summary produces one new summary that subsumes the old one. The label says how
many messages the summary represents.

### 4.4 When compaction runs automatically

Automatic compaction is on by default. Before each request is sent in a run,
the agent checks whether the projected context has reached the threshold:

- threshold = **80%** of the window (`--compact-at <percent>` / `compact_at`);
- the check runs once before each request. If used is at or above the threshold
  and automatic compaction is enabled, compaction runs, and then the request is
  sent; the check never loops, so a run cannot stall trying to shrink itself;
- the summarized context is targeted at **at most 50%** of the window, so the
  session has room for several more turns rather than compacting again
  immediately; if the kept tail alone is larger than that, older tool results in
  the tail are shortened (replaced with an `output omitted` marker) until the
  target is met;
- if compaction does not bring the context below the threshold — because the
  system prompt and the tool definitions alone are large, or because the
  summarizable part is tiny — further automatic compaction is disabled for the
  rest of the session and one warning says so. The indicator keeps warning, and a
  rejected request reports the provider's own error. Summarizing repeatedly for
  no gain is worse than sending and failing once.

Automatic compaction applies in every mode, including print and JSON, because it
protects the run itself; the interfaces differ only in how they report it (§4.6).

`--no-auto-compact` / `auto_compact = false` turns it off. With it off, the
indicator still warns, and a request that exceeds the window fails with the
provider's error plus advice to run `/compact` or `/new`. A manual `/compact`
still works: turning off the automatic trigger is not the same as losing the
ability to compact.

### 4.5 The `/compact` command

```
/compact [instructions]
```

- Compacts immediately when the console is idle.
- Submitted while a run is in flight, it is **queued** after the current turn,
  like a prompt: compaction needs a stable history, and aborting the user's work
  to compact would be surprising. The queue holds the command, so it is never
  sent to the model as a message.
- The optional text guides the summary ("keep the API changes and the failing
  test output"), and is passed to the summarization request as the user's
  preference.
- With too little conversation to summarize (only the kept tail remains), it
  reports that there is nothing to compact and does nothing.
- The command is available in the terminal UI and the web console. It is not a
  mode or a flag: one-shot and JSON runs compact automatically when needed, and
  have no command line to type into.

### 4.6 What the user sees

- **Before**: the footer shows `ctx 105k/128k 82%` in the error colour.
- **During** an automatic compaction, the run status becomes `compacting…` with
  the same spinner as a normal run; a prompt submitted then is queued as usual.
- **After**, a transcript block records what happened:

  ```
  ◆ context compacted
    18 messages summarized · 105k → 24k tokens · the last 3 turns kept
  ```

  The block is informational, not an error. When a tool result in the kept tail
  had to be shortened to meet the target, the block says so
  (`2 tool results shortened`). The summary text itself is available with
  `/context summary`.
- The **visible transcript is not rewritten**: the messages that were summarized
  stay on screen for the rest of the session. The transcript is the user's record
  of what happened; compaction is a change to what the model is sent, not a
  deletion of the conversation.
- In **JSON mode**, compaction emits a `compaction` event carrying the number of
  messages summarized, the token counts before and after, whether tool results
  were shortened, and whether it was automatic or requested; the usage events
  already on that stream carry the figures the indicator shows.
- In **print mode**, a single notice goes to stderr, where tool activity already
  goes, so stdout stays the answer alone.

### 4.7 Failure and interruption

- A compaction request uses the normal retry policy for transient provider
  failures (`basic-functions` §2.5).
- If summarization fails, the active context is left **unchanged** and the
  failure is reported. An automatic compaction that fails does not block the run:
  the request proceeds with the uncompacted context, and its own outcome is what
  the user sees. A manual `/compact` reports the error and does nothing else.
- If the summarization request itself would exceed the window, the conversation
  is summarized in two or more parts (each part summarizing what came before it
  plus its own slice, oldest first) and the results are combined into one
  summary. This is the case where the context is already at the limit before
  anything is compacted, so it is the path that must not fail.
- Aborting (`esc`) during a compaction cancels it and leaves the context
  unchanged.
- Compaction never runs while a tool call is outstanding, and never runs in the
  middle of a streamed answer; the cut is always between turns.

## 5. Configuration

| Setting | Flag | Config key | Default |
|---|---|---|---|
| Context window override | `--context-window <tokens>` | `context_window` | 0 = discover (§3.3) |
| Automatic compaction | `--auto-compact` / `--no-auto-compact` | `auto_compact` | on |
| Threshold | `--compact-at <percent>` | `compact_at` | 80 |
| Kept tail | — | — | 3 turns |
| Summary target | — | — | 50% of the window |

All of these resolve with the existing precedence (**flag > environment > config
file > default**) and appear in `--show-config` with their source, like every
other setting. `context_window` also accepts a size suffix (`128k`, `1m`) for
convenience, since that is how model limits are usually written.

No new environment variables are introduced: these three settings are chosen once
per session and belong in the config file, not in a shell profile. The existing
`HAL_*` variables are unchanged.

With `--no-session`, compaction still works — it changes what is sent to the
model, which is independent of whether the conversation is stored — but nothing
is recorded, so a compaction cannot be seen again after the process exits.

Invalid values are rejected the way other settings are, naming the source:
`context_window` must be positive, and `compact_at` must be between 1 and 100.
A `compact_at` below 70 (the severity warning threshold) is accepted and simply
means the indicator jumps from normal to critical, which is the user's choice to
make.

`--context-window` is also the escape hatch for a service whose real limit is
larger than the built-in table assumes: setting it is always correct, whereas a
wrong discovered limit is a guess.

## 6. Interaction with existing behaviour

### 6.1 Sessions

The stored session log remains complete: compaction does not delete the messages
it summarized, so the file still records the whole conversation and can be
audited or read by other tools. A compaction is recorded as its own entry
(summary text, the range of messages it replaces, token counts, time), and on
resume the active context is reconstructed from the summary onwards — the same
context the running console had. Storing the range as well as the text is what
makes that reconstruction exact rather than inferred. The rest of the file stays
on disk but is not sent to the model.

Consequences:

- Resuming a compacted session does not restore the summarized messages into the
  model's context; it restores the summary. This is what makes resuming survivable
  for a long session.
- The transcript rebuilt after a resume or a session switch shows the summary and
  the messages after it, and says how many earlier messages are in the file but
  not in the active context. It does not replay the summarized messages.
- The session switcher marks a compacted session, so it is clear why it is
  shorter than its file.

### 6.2 `/clear`, `/new`, and session switching

- `/clear` stays as it is: it empties the visible transcript and touches neither
  the conversation nor the context indicator.
- `/new` starts a fresh conversation and session; the indicator resets, and no
  summary carries over.
- Switching sessions aborts a run in flight, as it does today, and recomputes
  both the limit (from the model in use) and the used figure (from the restored
  context, marked `~` until the first response).

### 6.3 Model switching

`/model <name>` changes the limit and therefore the percentage and the
auto-compaction threshold immediately. If the new window is smaller than the
current context, the next request is preceded by an automatic compaction (if
enabled); if compaction is disabled, the request is attempted and its failure is
reported with the usual advice.

### 6.4 What "context" means versus what is stored

Deliberately, the active context and the stored log can differ after a
compaction: the log is complete, the context is what the model is sent. The
footer and `/context` always report the active context, never the log. This is
the one place where "what you see in the session" and "what the model sees" are
not the same thing, and the interface says so rather than hiding it.

### 6.5 Where the logic lives

The feature is deliberately split so that nothing about it is interface-specific:

- **What the model is sent** — the summary, the kept tail, the message boundary
  — is decided in the agent loop, so every mode behaves the same way.
- **The figures** — used, limit, percentage, whether they are measured or
  estimated — are reported as part of the run's event stream, so the terminal
  UI, the web console, and a JSON consumer all read the same numbers.
- **The commands** (`/context`, `/compact`) belong to the two consoles, and are
  thin wrappers over those two things.

This is why the feature cannot be implemented in one front end: the loop has to
know the limit, and the interfaces have to be told about it.

### 6.6 Security and trust

- The summary is model output and is subject to the same trust boundary as any
  assistant text: it can be wrong, and prompt-injected content from a file that
  was read earlier can survive into it. It is labelled as a summary, not as
  instructions, so a model reading it is told what it is. `/context summary` shows
  the user the same text, which is what makes the risk auditable rather than
  hidden.
- Compaction is lossy. Details the summary omits are gone from the model's
  context for the rest of the session, although they remain in the session file.
- Compaction sends part of the conversation to the provider a second time, as a
  summarization request; the privacy properties are those of any other request.

### 6.7 Cost

Compaction is an extra provider request, and therefore an extra cost, at the
moment the context is largest. This is the intended trade: one expensive request
now, in exchange for continuing the session. The indicator and the notification
report what it saved.

## 7. Non-goals

Not part of this feature, and not to be added by implementing it:

- **Exact tokenization.** No bundled tokenizer, no per-vendor token counting; the
  figure is measured where the provider reports it and estimated elsewhere.
- **A vendor matrix of model limits.** The built-in table is a convenience with a
  conservative default, not a catalogue to maintain.
- **Cross-session memory, retrieval, or vector storage.** Compaction summarizes
  the current conversation; it does not create a knowledge base.
- **Selective per-message pinning, editing, or re-fetching.** The user can guide
  the summary with `/compact <instructions>`, and nothing more.
- **Undo.** A compaction cannot be reversed within the active context; the
  original messages remain readable in the session file.
- **Cost accounting.** Token counts are reported, as they are today; money is
  not.
- **Automatic model switching** to a larger-window model when the context fills.
- **Rewriting the visible transcript** on compaction.

## 8. Impact on the existing design, once implemented

When this is implemented, `design/features/basic-functions` changes in these
places, and this document is the reference for the new text:

- §2 (the agent loop) notes that a request may be preceded by a compaction, and
  that §2.5's retry policy covers the summarization request too.
- §4.1 and §4.2 (the terminal and web footers) gain the context segment; the
  §4.1 footer description currently ends with the turn count.
- §5 (sessions) notes that a compacted session keeps its full log and resumes
  from the summary, and that the switcher marks it.
- §6.1 (slash commands) gains `/context` and `/compact`.
- §6.3 (config file) gains `context_window`, `auto_compact`, and `compact_at`;
  §6.4 (environment) is unchanged, as this feature adds no variables.
- §9 (explicit non-features) drops "context-window tracking"; the remaining
  exclusions stand, and this feature's own non-goals (§7 here) are added to that
  list rather than left implicit.
- The README gains the same surface: the footer description, the two commands,
  the three settings, and a section on compaction; the CHANGELOG gets one entry.

The matching technical design — how the limit is discovered and cached, how the
estimate is computed and corrected, where the threshold check sits in the agent
loop, how a compaction entry is represented in the JSONL format, and how the
front ends render the new state — is a separate technical item, written before
the code. Nothing in this document can be implemented without it, since the
feature touches the agent loop, the provider layer, the session format, and both
front ends at once.
