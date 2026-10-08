# Getting help

1. **Read first.** The [README](./README.md) covers install, reports and the FAQ; [docs/design.md](./docs/design.md) specifies every behaviour.
2. **Ask callmeter what it missed.** `/callmeter:report faults` lists the events it could not record and the snippets it could not parse; `/callmeter:report coverage` lists the transcripts on disk it never recorded.
3. **Open an issue.** Use the [issue templates](https://github.com/rezzminator/callmeter/issues/new/choose) for a question, a bug or an idea. For a bug, include:
   - the callmeter version (the `bin/{version}` directory under `CALLMETER_HOME`);
   - your Claude Code version (`claude --version`);
   - your OS and CPU architecture;
   - the output of the report that shows the problem.

Before you paste report output, read it: reports hold file paths, commands and session ids, and the `sessions` report holds your host name and time zone. Remove anything you do not want public.

Security and privacy problems go through [SECURITY.md](./SECURITY.md), never a public issue.
