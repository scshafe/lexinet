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

1. Mint the key (above) and give it to `stack enroll` on stdin (one line).
2. The first deploy builds the image and stops before `compose up` ("enroll the
   node, then deploy again"). Run `stack enroll`: it runs `stack.toml [node]
   enroll` (`/usr/local/bin/lexinet tailnet enroll --state-dir
   /var/lib/lexinet/tsnet --hostname lexinet-study`) with `./state/tsnet` bound,
   the key on stdin and nothing else.
3. Confirm the node `lexinet-study` appears with only `tag:lexinet-study`.
4. Deploy again. `serve` finds the enrolled directory and opens the listener.
5. The directory is an identity secret: `[backup] tailscale = true` archives
   it; never commit or print its contents.
