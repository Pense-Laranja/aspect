# Aspect

Aspect is a system of agents that builds software from a formal
specification and then checks whether what it built achieves the goals the
specification states. The specification declares the *intent* of the system
and of every module, plus the *goals* the finished system must reach.
Separate agents write the code, write the tests from the spec, run them for
real, and judge, goal by goal and with cited evidence, whether the
implementation honours the intent.

## Documentation

- [Spec language reference](SPEC.html): every section, field, notation rule
  and validation rule of format version 1.
- [Architecture](ARCHITECTURE.html): why the agents are separated, how a
  run flows, tiers, language profiles, and how an existing system is
  imported.
- [Review agents](design.html): the subagents that keep the spec true,
  review and attack every change, and write docs, test data and videocast
  scripts; their shared conventions and how review gates run.

## Where to start

```sh
go install github.com/Pense-Laranja/aspect/cmd/aspect@latest
aspect validate examples/inventory/aspect.yaml
aspect plan     examples/inventory/aspect.yaml
aspect run      examples/inventory/aspect.yaml -out ./out
```

Examples in the repository:

| Example | Shows |
|---|---|
| [monolith_minimal](https://github.com/Pense-Laranja/aspect/blob/main/examples/monolith_minimal/aspect.yaml) | the smallest spec that runs |
| [inventory](https://github.com/Pense-Laranja/aspect/blob/main/examples/inventory/aspect.yaml) | goals, invariants, scenarios, contracts |
| [warehouse_admin](https://github.com/Pense-Laranja/aspect/blob/main/examples/warehouse_admin/aspect.yaml) | a Postgres data model, a qor5 admin, an HTTP API |
| [shop_two_tier](https://github.com/Pense-Laranja/aspect/blob/main/examples/shop_two_tier/aspect.yaml) | a Go backend and a Swift iOS app in two tiers |
| [cloud_service_minimal](https://github.com/Pense-Laranja/aspect/blob/main/examples/cloud_service_minimal/aspect.yaml) | a Swift app over a hosted backend |

Source and issues: [github.com/Pense-Laranja/aspect](https://github.com/Pense-Laranja/aspect).
