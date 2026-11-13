# Team Design and Operations

[Kingsoft Cloud bare-metal assets](design/bare-metal-assets.md) records SSH entry points and CSI coverage without credentials.

This site collects team-specific designs, deployment records and operational guides separately from RLark's native documentation. Observations and validation results are snapshots; verify the current environment before acting.

The [Chinese index](index.md) lists all documents, including Chinese-only plans. English documents are available for the [Station API](design/station-api-contract.md), [Station identity protocol](design/station-identity-protocol.md), [shared storage and registry](design/shared-storage-registry-plan.md), [private registry operations](operations/private-registry.md), and [Station deployment snapshot](design/station-deploy/README.md).

Install the dependencies in `apps/rlark/docs/requirements.txt` from the repository root, then run `mkdocs serve -f mkdocs-team.yml`. Build with `mkdocs build --strict -f mkdocs-team.yml`. HTML files retain their own appearance and navigation and are not covered by the Markdown search index. Restrict access to any deployment because these documents contain environment-specific information.

Keep paired English and Chinese pages aligned, and record the validation date, applicable environment, and known limitations. Do not describe historical observations as current deployment or acceptance.

| Fleet | [NAS path contract](design/fleet-storage-paths.md) · [中文](design/fleet-storage-paths.zh-CN.md) | Fixed paths; migration and cross-site acceptance tracked separately |
