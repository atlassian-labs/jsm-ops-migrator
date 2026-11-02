# Security policy

Thanks for helping keep Atlassian's open source projects and the people who use them safe.

## Reporting a vulnerability

**Please don't open a public GitHub issue for a security problem.** Public issues are visible to everyone and can put users
at risk before a fix is ready.

## How to report

We use **GitHub Private Vulnerability Reporting** to allow security researchers to disclose vulnerabilities directly to
repository maintainers:

 1. Navigate to the **Security** tab of this repository.
 2. Click on **Advisories** under "**Reporting**".
 3. Click **Report a vulnerability** to submit your report privately.

A good report usually includes:

- What you found, and why you think it's a security issue
- Steps to reproduce, or a proof of concept
- The affected component (for example, a specific file in the repository, a module, or a connected endpoint)
- The impact you think it could have

## Scopes

### What We Consider a Vulnerability 

We consider a security vulnerability to be a flaw that could allow an attacker to compromise the confidentiality, integrity,
or availability of this project or its users. We welcome reports on the following issues affecting this repository:

### In-Scope

- Flaws in source code (for example, RCE, SQLi, XSS, Auth Bypass, Privilege Escalation).
- Exposed API keys, tokens, or credentials committed directly to this repository.
- Critical security flaws in core dependencies accompanied by a practical proof-of-concept (PoC) showing real-world impact
  in this project context.
- Insecure defaults or flawed setup guidelines provided in this project.
- Vulnerabilities in CI/CD workflows, GitHub Actions, build scripts, or repository release processes.

### Out-of-Scope

In line with Atlassian security requirements, the following types of findings are out of scope for this repository:

- Reports of vulnerable sub-dependencies without demonstrating how the vulnerability can be triggered through this specific
  repository.
- Raw reports from automated scanners or Static Application Security Testing (SAST) tools or generic CVE filings without a
  manual proof-of-concept demonstrating a real-world exploit.
- Stack traces, local error logs, or verbose output generated during local execution/development.
- Vulnerabilities in hosted Atlassian products or infrastructure. Please submit those to the
  [Atlassian Bug Bounty Program](https://bugcrowd.com/atlassian).

## Preferred Languages

We prefer all communications to be in English.

## Coordinated disclosure

Please give us a fair chance to investigate and ship a fix before you share details publicly. We're grateful to the researchers
who report responsibly and work with us on timing.
