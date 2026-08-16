# hostagent/tools

Instruments, not agent code. Nothing here is imported or executed by the agent; these are the scripts that answer "does the window actually do that", which pytest cannot, because the claims are about what Windows draws.

All PowerShell 7. Every one of them puts back what it moved (cursor position, window state, throwaway scheduled tasks) and writes its output to `$env:TEMP` by default, so none of them can leave an artefact in the repository.

| script | question it answers |
|---|---|
| `capture-window.ps1` | what does the agent's window look like right now |
| `capture-taskbar.ps1` | whose icon is on the taskbar button |
| `capture-tray-menu.ps1` | what does the tray menu ACTUALLY offer, live |
| `crop-image.ps1` | zoom in on any of the above |
| `read-console-buffer.ps1` | what did Textual paint, as text |
| `window-identity.ps1` | read/write the AppUserModel properties behind the taskbar button |
| `check-show-hide.ps1` | born hidden, shows on demand, close button still gone |
| `check-logon-task.ps1` | does a logon flash a console window |

## What was learned the hard way

Each of these cost an hour and none of them is discoverable from the failure.

- **Call `SetProcessDpiAwarenessContext(-4)` before measuring anything.** Otherwise the shell virtualises `GetWindowRect` for the measuring process: a 1815x921 window reports 877x493, `PrintWindow` fills the top left corner of a too-small bitmap, and the result reads as "the right hand side of the window is not being drawn".
- **`PrintWindow` asks the window to paint itself**, so it cannot see anything drawn on the screen DC over the top. The minimize-to-tray wireframe (`DrawAnimatedRects`) is invisible to it by construction. Use `CopyFromScreen` for anything that is not the window's own content, and accept that it captures whatever is in front.
- **This taskbar auto-hides**, so `Shell_TrayWnd`'s own rect is two pixels of wallpaper. Push the cursor into the bottom edge and derive the rect from the screen.
- **pystray builds one HMENU and reuses the handle** for every right-click, so iterating `icon.menu` from a test re-runs the `visible` callables and reports what they would say now, not what the live menu says. Only `Icon.update_menu()` rebuilds it. That is why `capture-tray-menu.ps1` exists.
- **The taskbar button is not drawn from the window icon.** `WM_SETICON` reaches the title bar, the window menu, Alt-Tab and the hover thumbnail. The button comes from the AppUserModelID the shell resolves for the window, which for a console is the process that opened it. See `set_taskbar_identity` in `../console.py`.
- **The identity is read when the button is created and never again.** Set it while the window is hidden, or hide and re-show to force a new button.
- **`WriteConsoleInput` does not reach Textual.** Injecting a key event into the agent's CONIN$ was tried and abandoned: Textual's win32 driver does not read the events back out that way, so there is no way to press `h` for it from outside. Drive the app through the tray, or through its HTTP API.
- **Find pystray's window by class, never by process.** The agent runs in a child `python.exe` of the process whose command line mentions `llama_agent`, so matching on the command line finds the launcher, which owns none of the windows.
