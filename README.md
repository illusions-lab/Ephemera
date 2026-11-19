# Ephemera
An ultra-lightweight control plane for ephemeral, stateless GitHub Actions runners. 

All you need is two things: a GitHub App and a server to run your runners. 

Everything else is handled automatically: provisioning virtual machines, executing jobs, and destroying environments when jobs finish. Every job starts fresh.

Based on:
https://github.com/actions/scaleset?utm_source=chatgpt.com
https://github.com/cloud-hypervisor/cloud-hypervisor?utm_source=chatgpt.com
