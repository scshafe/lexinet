# Enrolling the `lexinet-study` node

lexinet refuses ambient enrollment (`TS_AUTHKEY` and the other variables in
`internal/tailnet/constants.go`) and needs a pre-enrolled `0700` state
directory. Enrollment is therefore a separate, one-time step:

```sh
printf '%s' "$KEY" | lexinet tailnet enroll --state-dir ./state/tsnet --hostname lexinet-study
```

- The one-use key is read from **stdin only**: there is no key flag, and
  `TS_AUTHKEY`/the other ambient variables are rejected here exactly as `serve`
  rejects them. It is never printed or logged; errors carry codes, not text from
  Tailscale.
- `--state-dir` is created `0700` if absent and **must be empty**; re-enrolling
  an existing identity is refused (delete the directory deliberately to redo it).
- It brings the node up once (`--timeout`, default 30s, max 2m), waits for
  `Up`, closes it and exits. `serve` is never involved.
- The node's tag comes from the key: mint it **single-use, preauthorized,
  non-ephemeral, with `tag:lexinet-study`**. Hostname `lexinet-study`.
  MagicDNS and HTTPS must be on (they are).

## Provisioning procedure (owner / infra)

1. Mint the key (above) and place it only where the one-shot step reads it.
2. Run the enrollment as a one-shot container from the same image as `serve`,
   as the service uid (65532), with `./state/tsnet` bind-mounted at the state
   path and the key on stdin:
   `docker run --rm -i --user 65532:65532 -v <stack>/state/tsnet:/state IMAGE tailnet enroll --state-dir /state --hostname lexinet-study < keyfile`
   (the directory must exist, empty, mode `0700`, owned by 65532).
3. Delete or blank the key. Confirm the node `lexinet-study` appears with only
   `tag:lexinet-study`.
4. Start the stack. `serve` finds the enrolled directory and opens the listener.
5. The directory is an identity secret: back it up with the stack and never
   commit or print its contents.

Infra's `tools/provision` mints exactly one `TS_AUTHKEY` per stack for a
sidecar; this stack needs the key to go to the step above instead, and not into
the stack's `.env`.
