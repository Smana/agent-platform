# agent-platform

Agent Factory: the room broker and bridge that let agents and humans collaborate in a shared room.
Deployed by [cloud-native-ref](https://github.com/Smana/cloud-native-ref).

| Binary | Image |
|---|---|
| `room-broker` | `ghcr.io/smana/room-broker` |
| `room-bridge` | `ghcr.io/smana/room-bridge` |

## Develop

```bash
mise install    # toolchain pinned in mise.toml
task check      # every gate CI runs
```

Each push to a PR publishes `v<next-patch>-pr<N>.<sha8>` images, `<sha8>` being the PR head's.
A `v*` tag publishes the release images.

## License

Apache-2.0
