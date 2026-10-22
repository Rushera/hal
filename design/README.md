# hal design documents

This directory is the design specification of hal: what the program does, and
how it is built. The code is written against these documents, and documents are
updated before (or together with) the code they describe.

These documents are written and maintained by people. AI agents read them but do
not write them — see `AGENTS.md` at the repository root for the rule and for the
rest of the working agreements.

## Layout

```
design/
  features/                  what hal does
    basic-functions/         the current feature set, coarse to fine
    context-management/      proposed: context indicator and compaction
  technical/                 how hal is built
    architecture/            the current technical architecture
```

The split is strict: a **feature** document answers "what does the user get, and
what does it do", a **technical** document answers "how is that implemented".
Both are written from coarse to fine — an overview first, then the parts, then
the details that matter to whoever changes the code.

Each item is one folder, and its `README.md` is the entry point:

- One feature item per folder: a feature is never spread across folders, and two
  feature items are never merged into one folder.
- One technical item per folder, with the same rule.
- Supporting files (diagrams, tables, worked examples) go inside the folder of
  the item they belong to.
- Adding an item means adding a folder and a line to the index below.

A feature item and its technical counterpart are two folders, never one: the
feature document states what happens, the technical document how. A proposed
feature is listed in the index with its status, and so is the technical document
it still needs.

## Index

### Features

| Item | Scope |
|---|---|
| [basic-functions](features/basic-functions/README.md) | Everything hal currently does: the agent loop, the tools, the four interfaces, sessions, configuration, and the guardrails around them. |
| [context-management](features/context-management/README.md) | **Proposed, not implemented.** A context-window indicator (used / limit) and compaction: automatic when the window fills, or on `/compact`. |

### Technical

| Item | Scope |
|---|---|
| [architecture](technical/architecture/README.md) | Package layout, the interfaces between packages, concurrency, data formats, configuration resolution, retry policy, and the testing strategy. |
| context-compaction *(not written yet)* | The technical design of `features/context-management`: limit discovery, token estimation, the threshold check in the agent loop, the compaction entry in the JSONL format, and how the front ends render the new state. Required before that feature can be implemented. |

The index above lists only items that exist today. When a new item is designed
but not yet written, it is listed in the index of the subtree it belongs to with
an explicit "not written yet" note, so a gap is visible rather than silently
missing.
