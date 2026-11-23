# JikuNode on Windows

JikuNode runs on Windows 10 and 11 (amd64 and arm64) as a per-user startup
app. No administrator rights or Windows service are involved.

- `jikunode.exe` is the same command-line agent as on Linux.
- `jikunode-tray.exe` is the JikuNode app. It starts when you sign in (from
  the `JikuNode` value under `HKCU\Software\Microsoft\Windows\CurrentVersion\Run`),
  runs `jikunode.exe run` without a console window, and restarts it five
  seconds after it exits. It has a window and a notification-area icon.

## Install

1. Download `jikunode_<version>_windows_<arch>.zip`, `checksums.txt`,
   `checksums.txt.sig` and `install-user.ps1` from the release.
2. Run the installer from PowerShell. With `-Checksums` it checks the zip's
   SHA-256 against `checksums.txt` (verify the signature on `checksums.txt`
   yourself first):

   ```powershell
   powershell -ExecutionPolicy Bypass -File .\install-user.ps1 .\jikunode_<version>_windows_amd64.zip -Checksums .\checksums.txt
   ```

   The script copies both programs to `%LOCALAPPDATA%\Programs\jikunode`, adds
   that folder to your PATH, registers autostart, adds a JikuNode entry to the
   Start menu and starts the app with its window open.
   Options: `-Port <n>` if port 16888 is already used on this computer, and
   `-NoStart` to start at the next sign-in instead of now.
3. Bind the node: in the JikuNode window, type the binding code from the
   provider console (and a name for this computer, if you like) and press
   **Bind**. The node starts and the icon turns green once it is connected.
   From a terminal, `jikunode bind <code> --name office-pc` does the same.

## Portable copy (no install)

To run JikuNode from a folder you choose, such as a USB drive, without the
installer:

1. Download `jikunode_<version>_windows_<arch>_portable.zip` (check it
   against `checksums.txt`) and extract it to that folder.
2. Start `jikunode-tray.exe` from there, enter the binding code and press
   **Bind**.

The portable zip holds the two programs and an empty file named
`jikunode.portable`. That file is what makes the copy portable: the config,
node key, logs and preferences then live in a `data` folder next to the
programs, not in your user profile, and the copy is not added to PATH or the
Start menu. You can turn any copy into a portable one by creating that file
next to `jikunode.exe`. Moving the folder to another computer moves the node
identity with it.

"Start JikuNode when I sign in to Windows" still works for a portable copy as
long as the folder stays at the same path (for a USB drive: the same drive
letter). To remove a portable copy, quit JikuNode, untick start at sign-in
first if you turned it on, and delete the folder.

On a drive formatted FAT32 or exFAT (common for USB sticks) Windows keeps no
access lists, so the node key in `data` cannot be limited to your account:
anyone who has the drive can read it and act as this node. Prefer an
NTFS-formatted drive, or keep the drive safe.

## The JikuNode window

Click the JikuNode icon in the notification area, or start JikuNode from the
Start menu, to open the window. It shows whether the node is connected, the
hub, the node id and version, and has:

- **Start node / Stop node**: Stop disconnects from the hub and stops the
  node; it stays stopped until you press Start (or restart JikuNode with
  "Connect automatically" on). Closing the window does not stop the node.
- **Pause / Resume**: asks the hub to stop or resume sending traffic, like
  `jikunode pause`, while the node stays connected.
- **Start JikuNode when I sign in to Windows**: the Run key entry. At sign-in
  JikuNode starts in the notification area without opening the window.
- **Connect to the hub automatically when JikuNode starts**: when off,
  JikuNode starts with the node stopped and waits for Start. Saved in
  `%LOCALAPPDATA%\jikunode	ray.json`.
- **Binding code** and **Name** boxes with a **Bind** button, shown while the
  node is not bound yet.
- **Open status page** and **Open log folder**.

Together the two preferences decide what happens when Windows starts: both on
means the node is connected as soon as you sign in.

## Tray icon

| Colour | Meaning |
|---|---|
| Green | Connected and serving |
| Amber | Connecting, starting or paused |
| Red | The hub cannot be reached (the menu shows the last error) |
| Grey | Stopped, not bound yet, or about to restart after a crash |

Right-click the icon for the menu: Open JikuNode, Start/Stop node,
Pause/Resume, Restart node, the status page and log folder, the two
preferences, and Quit. Quit stops the node and closes JikuNode until you start
it again from the Start menu or sign in again (if start at sign-in is on).

## Files

Installed copy (a portable copy keeps all of this in `data\config` and
`data\state` next to its programs):

| What | Where |
|---|---|
| Programs | `%LOCALAPPDATA%\Programs\jikunode` |
| Config, node id and private key | `%APPDATA%\jikunode` |
| Node and tray logs (`node.log`, `tray.log`) | `%LOCALAPPDATA%\jikunode\logs` |
| App preferences (`tray.json`) and upgrade lock | `%LOCALAPPDATA%\jikunode` |

The config folder and key get an access list that allows only your account
and SYSTEM. The node refuses to load a key that any other account can read,
the Windows counterpart of the `0600` check on Linux.

## Upgrade and uninstall

Hub-triggered upgrades work as on Linux: the node installs the signed release,
exits, and the tray app starts the new version (restarting itself too when the
tray binary changed). `jikunode upgrade` does the same on demand.

```powershell
jikunode uninstall           # stop JikuNode; remove autostart, Start menu entry, PATH entry and programs
jikunode uninstall --purge   # also remove the config, key and logs
```

## Limits

- The node runs only while you are signed in. For an always-on machine, turn
  on automatic sign-in, or run `jikunode run` from a scheduled task or a
  service wrapper of your choice.
- Each Windows user who installs JikuNode gets their own node identity.
