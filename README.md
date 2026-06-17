# zcoms-bridge

The **bridge** component for [zcoms](https://github.com/Zouriel/zcoms): the
interactive agent-driving session state machine (locations, session resume,
`chat`, interactive triage, file handling) for allow-listed users.

It owns no Telegram session — the core daemon does. The daemon pushes allow-listed
users' messages here over a subscribe stream and this replies via IPC, so it's
pure-Go (no cgo/TDLib). When this component is running, the daemon routes to it;
when it's stopped, the daemon falls back to handling the bridge in-process.

## Install
```sh
zc install bridge      # downloads the prebuilt binary + sets up the service
```
