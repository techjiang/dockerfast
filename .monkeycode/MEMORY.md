# User Instruction Memory

This file records user instructions, preferences, and teachings for reference in future interactions.

## Format

### User Instruction Entry
User instruction entries should follow this format:

[User Instruction Summary]
- Date: [YYYY-MM-DD]
- Context: [Mentioned scenario or time]
- Instructions:
  - [Content of user teaching or instruction, described line by line]

### Project Knowledge Entry
Entries discovered by the Agent during task execution should follow this format:

[Project Knowledge Summary]
- Date: [YYYY-MM-DD]
- Context: Discovered by Agent while performing [specific task description]
- Category: [Operations & Deployment|Build Methods|Testing Methods|Troubleshooting & Debugging|Workflow & Collaboration|Environment Configuration]
- Instructions:
  - [Specific knowledge points, described line by line]

## Deduplication Strategy
- Before adding a new entry, check for similar or identical instructions.
- If a duplicate is found, skip the new entry or merge it with the existing one.
- When merging, update the context or date information.
- This helps avoid redundant entries and keeps the memory file tidy.

## Entries

[Project Knowledge Summary]
- Date: 2026-10-09
- Context: Discovered by Agent while building and verifying the DockerFast pull-through registry proxy
- Category: Environment Configuration
- Instructions:
  - The development environment has no Docker daemon/CLI available, so DockerFast cannot be verified by a real `docker pull`. End-to-end verification uses Go `httptest` upstreams plus a local Python `http.server` mock registry.
  - The project intentionally has zero third-party Go dependencies (standard library only), so `go build ./...` and `go test ./...` work fully offline with no `go mod download` needed.
  - Use a remote/domestic mirror endpoint only by editing the default upstream's `endpoint` in the config; never hardcode credentials into the repo.
