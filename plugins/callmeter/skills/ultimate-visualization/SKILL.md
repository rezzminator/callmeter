---
name: ultimate-visualization
description: "Build one striking, animated Artifact of what happened in your Claude Code history, from the store the hooks fill: an open greenfield where Claude finds the data's real shape and chooses the visualization itself, with every chat, prompt, agent and tool call open to inspection. Optional focus text steers it."
argument-hint: "[focus]"
disable-model-invocation: true
allowed-tools: Bash, Read, Write, Edit
---

# callmeter ultimate visualization

Visualize what happened in the past, from the data callmeter recorded: the work Claude Code did on this machine, seen as a whole and as it flowed. One Artifact, in whatever form you find most striking. An open greenfield: no template, no menu of chart types, no layout handed to you.

The user's focus: `$ARGUMENTS`

With none, take the whole history and your own angle.

## The point of view

The work and its flow, from the inside: who did what, in what order, how it nested and connected, how it grew and where it surprised. How the recorded work is organised and linked is part of the picture. The container that holds it (file size, pages, storage mechanics) and row counts as the subject are not: a picture of the box is not a picture of what happened.

Every recorded thing is reachable in detail: each chat, prompt, agent run and tool call can be found in the work and opened down to its own record, with its fields, its place in the tree, its time and its size. Spectacle never replaces inspectability. A viewer who can only admire the shape and cannot ask "which one is that?" is blind, and a first delivery that leaves them blind has failed however striking it looks.

## Before the first file

1. Check that this session can publish an Artifact: the Artifact tool and its design type, the route `/design` takes. If it cannot, say so now, before building anything, and ask whether to build a self-contained HTML file instead. Never hand over a file and call it an Artifact.
2. Load the `callmeter` skill (`/callmeter:callmeter`) for how to open the store: a snapshot copy, read-only, never the live file, never a write.
3. Explore the store and look at it. Find out what it holds, how the pieces connect, how it changed over the days, what is unusual. Read real rows and real distributions, print samples of structure, and look at the numbers themselves, not only their totals. The idea comes from what the data actually shows: a visualization chosen from the schema alone is a guess.

## What the page may carry

The store keeps no message content, yet it holds command text, file paths, working directories, error labels and titles. The published page carries structure, timing and counts only: never a command, a path, a prompt, a project or directory name, an error text or a chat title. Name sessions and agents by a short anonymous label. The user can ask for more; without that ask, this holds. The snapshot copy holds the whole store: say where it sits and offer to delete it.

Anonymous does not mean blind. Even with names withheld, the first delivery carries a drill-down inspector over every recorded entity, built from what is safe: times, durations, tool classes, sizes, ok or failed, token counts, the agent tree and the counts at each level.

When the user's focus or reaction asks to see the things themselves (what ran, which command, which file, which folder), ask once how much real identity the page may carry: structure only, names and roles, or everything including commands and paths. Build to that answer and nothing past it. When it includes real content: say so in a visible notice on the page, mask obvious secrets by pattern (keys, tokens, bearer strings, `KEY=value` assignments for secret-named keys) and count them for the report, say plainly that masking is by pattern and not a guarantee, keep the link private, and name all of it in the report. The ask is the user's call, never a default.

## Imagine, before the first file

1. Find what this work is in your own conception: the thing only you would make of it. Name once the version a competent default would make, so you can leave it.
2. Push past your first idea until one surprises you; an idea that would pass unnoticed among work of its kind is not the one yet. Among those you can make work, choose the one that stops its receiver.
3. Leap in kind, never in polish: take the most capable medium the target runs unless the brief fixes one: 3D over flat, live over static, explorable over fixed. Check the materials first (what the page can load, the target's limits); ambition that cannot load is a blank page.
4. Name the boldest element, the one its receiver remembers, and build the work around it. It opens the data and never covers it: the receiver must be able to travel from the boldest view to one exact record in it.
5. Go deeper than the brief, never wider: interaction, motion, colour, the edge case, the detail found on a second look, all inside the asked-for work.

## Build

- By your own hand: no sub-agent or other model makes, judges or critiques the work unless the user names one.
- Every design decision is yours, made on the spot: the user asked for your imagination, not a menu.
- Every detail is chosen: names, timing, colour, spacing, copy, and the empty, error and reduced-motion states.
- The work carries its own identity: your chat's voice, catchphrases and costume stay out of it.
- A fact the work needs that sits on disk is read, never asked for. A fact only the user holds is never invented: build around it and name it in the report.
- Follow the Artifact tool's own page rules for the design type, and keep the page usable at phone width.
- Publish the data as uploaded assets (`Artifact` with `asset: true`) and fetch it from the page, never inline: keep the page small, and split a heavy record into shards the page loads when a view needs them. A multi-megabyte inline page rendered an empty canvas in the real viewer while the same page with its data in an asset rendered. Once published, ask the user to confirm the real viewer shows it, because the local render cannot prove that.

## Look and raise

Meet the work as its receiver does, never the source alone. Render the page locally in a real browser when one is available and look at real frames at several moments and at desktop and phone widths; drive every interaction. The viewer's own frame (its sandbox and content rules) cannot be checked from here: say so.

Then judge as the harshest expert in the field: where it is timid, where generic, what a first-time viewer will read it as, what a version ten times bolder would do. Raise the weakest, sharpen the strongest, and look at every change before it ships. Stop when a whole look finds only polish.

A finding that the boldest element is too much, too strange or misreadable raises it until it reads, never tames or cuts it; only the user takes it out.

## Deliver

Publish the Artifact and open it for the user. Report plainly: what it is, the idea behind it in a few lines, how to use it, what was verified and on what, what was not, and what the data could not show. The work carries the ambition; the report carries none. The page is private until the user shares it.

A reaction to the delivered work is read by its content, never by its tone: an ambiguous one keeps the boldest element and asks which way.

A reaction that the work shows too little ("I feel blind", "I expected to see each prompt, agent and call") is a finding about detail, not about taste: the answer is more reachable detail, never more polish. Ask the identity question above once, then rebuild the way in so that every entity opens to its record. If the user orders a new idea from scratch, the old boldest element is theirs to take out; the new work still follows every rule here.

## Rules

- A budget the user sets binds, and the report names what it cut; otherwise calls go to making and looking, never to ceremony.
- This raises the work, never the permissions: every project rule and permission setting holds unchanged.
- The store is only ever read, from a snapshot: never run `callmeter redact` or write to it.
