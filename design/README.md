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
  features/            what hal does
    basic-functions/   the current feature set, coarse to fine
  technical/           how hal is built
    architecture/      the current technical architecture
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

## Index

### Features

| Item | Scope |
|---|---|
| [basic-functions](features/basic-functions/README.md) | Everything hal currently does: the agent loop, the tools, the four interfaces, sessions, configuration, and the guardrails around them. |

### Technical

| Item | Scope |
|---|---|
| [architecture](technical/architecture/README.md) | Package layout, the interfaces between packages, concurrency, data formats, configuration resolution, retry policy, and the testing strategy. |

The index above lists only items that exist today. When a new item is designed
but not yet written, it is listed in the index of the subtree it belongs to with
an explicit "not written yet" note, so a gap is visible rather than silently
missing.
