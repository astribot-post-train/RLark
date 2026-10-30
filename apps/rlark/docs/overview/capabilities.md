# Core Capabilities

RLark provides a unified control plane for heterogeneous embodied-intelligence infrastructure.

- **Multi-cluster resource management** — onboard multiple runtimes and view usable compute and embodied devices consistently.
    - **Kubernetes Runtime — Preview:** implemented and suitable for evaluation; production stability is not yet guaranteed.
    - **Docker Runtime — Planned:** Docker container lifecycle is not implemented.
    - **Raw Runtime — Experimental:** A pre-provisioned Node can run one trusted process Task through the scoped Agent/supervisor path. Ray/RLinf groups, replicas, logs and onboarding are not supported.
- **Job orchestration** — describe distributed jobs as coordinated Tasks across available compute resources.
- **Cross-cluster networking** — connect workloads through TUN, gVisor netstack, and SSH tunnels without requiring direct inbound connectivity.
- **Interactive development** — inspect Workers, copy SSH commands, and open WebTerminal sessions from the console.
- **Embodied Runtime** — expose robots and cameras as schedulable resources through device plugins and runtime controllers.
- **Control and governance** — manage scheduling, metadata, access, storage, and operational status from the administrator console.

Continue with [Core Concepts](../concepts.md), then follow the [Quick Start](../quickstart.md) for an end-to-end deployment.
