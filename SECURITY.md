# Security policy

## Supported versions

Only the latest release receives security fixes. The database lives on the local
machine and is created with mode `0600` inside a `0700` directory; task contents
are never sent over the network by taskctl itself.

## Reporting a vulnerability

Please **do not open a public issue**. Use GitHub's private vulnerability
reporting on the repository's Security tab ("Report a vulnerability"):

<https://github.com/AlekseyShibayev/taskctl/security/advisories/new>

Include the taskctl version (`taskctl version`), the operating system, the
command or MCP request that triggers the problem, and, when possible, a minimal
reproduction. You will get an acknowledgement within seven days.

## Scope

In scope:

- reading task data without permission from the same machine;
- a crafted MCP message that crashes or hangs the server;
- SQL injection or a lost update through the MCP interface;
- privilege escalation through the installer or the build.

Out of scope:

- other local users with the same account/root access;
- secrets placed inside task names or descriptions by the user;
- denial of service by exhausting disk space with legitimate writes.
