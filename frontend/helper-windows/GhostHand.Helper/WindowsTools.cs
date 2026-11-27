using Microsoft.Win32;
using System.Diagnostics;
using System.Globalization;
using System.IO;
using System.Runtime.InteropServices;
using System.Security.Principal;
using System.Text.Json;
using System.Windows;
using System.Windows.Automation;

namespace GhostHand.Helper;

internal sealed class WindowsTools
{
    private const int MaxElements = 90;
    private const uint OwnerWindow = 4;
    private readonly Dictionary<int, AutomationElement> elements = new();
    private IntPtr inspectedWindow;
    private DateTimeOffset inspectedAt;
    private int nextElementId = Random.Shared.Next(1, int.MaxValue);
    private string? owningTask;
    private (IntPtr Handle, int ProcessId)? approvedEditor;
    private static readonly HashSet<string> NavigationKeys = new(StringComparer.OrdinalIgnoreCase)
    {
        "Tab", "Left", "Right", "Up", "Down", "Home", "End", "PageUp", "PageDown", "Escape", "Esc",
    };
    private static readonly HashSet<string> CalculatorButtons = new(StringComparer.Ordinal)
    {
        "plusButton", "minusButton", "multiplyButton", "divideButton", "equalButton", "decimalSeparatorButton", "negateButton",
    };

    public Task<object> CallAsync(string task, string name, JsonElement args, string expectedWindow, bool approved, CancellationToken token)
    {
        token.ThrowIfCancellationRequested();
        if (owningTask != task)
        {
            owningTask = task;
            approvedEditor = null;
        }
        if (!DesktopCapture.IsDefaultDesktop())
        {
            throw new CaptureException("E_DESKTOP_LOCKED");
        }

        var original = SetThreadDpiAwarenessContext(new IntPtr(-4));
        try
        {
            if (name == "screen.observe")
            {
                var windowOnly = Flag(args, "window_only");
                var id = String(args, "window_id");
                var window = ResolveWindow(id);
                int? index = args.TryGetProperty("monitor", out var monitor) ? monitor.GetInt32() : null;
                var display = DesktopCapture.SelectDisplay(index, windowOnly || id.Length > 0 ? window.Handle : IntPtr.Zero);
                var scene = BuildObservation(window, args, Flag(args, "annotate"), token, display);
                return CaptureObservationAsync(scene, windowOnly, display.Handle, token);
            }
            return Task.FromResult(Execute(name, args, expectedWindow, approved, token));
        }
        finally
        {
            if (original != IntPtr.Zero)
            {
                SetThreadDpiAwarenessContext(original);
            }
        }

        async Task<object> CaptureObservationAsync(Observation scene, bool windowOnly, IntPtr monitor, CancellationToken currentToken)
        {
            var jpeg = await DesktopCapture.CaptureAsync(currentToken, scene.Marks, windowOnly ? scene.Region : null, monitor);
            return new { jpeg, data = scene.Data };
        }
    }

    private object Execute(string name, JsonElement args, string expectedWindow, bool approved, CancellationToken token)
    {
        switch (name)
        {
            case "browser.ext_install":
                return ChromeExtensionInstaller.InstallUnpackedExtension(String(args, "browser"), String(args, "extension_dir"),
                    String(args, "manifest_path"), String(args, "install_token"), token);
            case "browser.ext_remove_uia":
                return ChromeExtensionInstaller.RemoveExtensionViaUIA(String(args, "browser"), String(args, "install_token"), token);
            case "browser.ext_close_uia":
                return ChromeExtensionInstaller.CloseManagementTab(String(args, "browser"), String(args, "install_token"), token);
            case "device.get_status":
                return new { data = BuildObservation(Foreground(), args, false, token, includeDisplays: true).Data };
            case "ui.inspect":
                return new { data = BuildObservation(ResolveWindow(String(args, "window_id")), args, true, token).Data };
            case "window.list":
                return ListWindows(args, token);
            case "window.control":
                return ControlWindow(args, approved, token);
            case "ui.wait_for":
                return WaitFor(args, token);
            case "app.launch":
                return Launch(args, approved, token);
            case "terminal.open":
                return OpenTerminal(args, approved, token);
            case "clipboard.manage":
                return ManageClipboard(args, approved);
            case "device.power_screen":
                return PowerScreen(args, approved);
        }

        var target = Foreground();
        var identity = (target.Handle, target.ProcessId);
        if (approvedEditor != identity)
        {
            approvedEditor = null;
        }
        if (args.TryGetProperty("element_id", out _) && expectedWindow != WindowId(target.Handle))
        {
            throw new CaptureException("E_UI_INSPECT_AGAIN");
        }
        token.ThrowIfCancellationRequested();

        int[]? clickedXY = null;
        switch (name)
        {
            case "ui.click":
                if (approved || Flag(args, "hover_only"))
                {
                    clickedXY = Click(target, args, token);
                }
                else if (TryRoutineClick(target, args, token, out var routineXY))
                {
                    clickedXY = routineXY;
                }
                else
                {
                    throw new CaptureException("E_CONFIRM_REQUIRED");
                }
                break;
            case "ui.input_text":
                var element = TryResolveEditor(target, args);
                var text = String(args, "text");
                var mode = String(args, "mode");
                var submit = Flag(args, "submit");
                var routineText = !submit && !text.Any(char.IsControl);
                if (!approved && !(target.TrustedNotepad && !submit))
                {
                    if (!routineText)
                    {
                        throw new CaptureException("E_CONFIRM_REQUIRED");
                    }
                    if (approvedEditor != identity)
                    {
                        throw new CaptureException("E_CONFIRM_EDIT_WINDOW_REQUIRED");
                    }
                }
                EnterText(target, element, text, mode, Flag(args, "clear_first"), submit, token);
                if (approved && routineText)
                {
                    approvedEditor = identity;
                }
                break;
            case "ui.press_key":
                var keys = args.GetProperty("keys").EnumerateArray().Select(value => value.GetString() ?? "").ToArray();
                var repeat = Number(args, "repeat", 1);
                if (!approved && !IsRoutineKey(target, keys))
                {
                    throw new CaptureException("E_CONFIRM_REQUIRED");
                }
                WindowsInput.Keys(keys, token, repeat);
                break;
            case "ui.scroll_swipe":
                if (!approved && args.TryGetProperty("from_xy", out _))
                {
                    throw new CaptureException("E_CONFIRM_REQUIRED");
                }
                Scroll(target, args, token);
                break;
            default:
                throw new CaptureException("E_CAP_UNSUPPORTED");
        }

        var after = TryForeground() ?? target;
        if (inspectedWindow != after.Handle)
        {
            Invalidate();
        }
        return new
        {
            data = new
            {
                status = "ok",
                clicked_xy = clickedXY,
                active_app = after.App,
                window_title = after.Title,
                window_id = WindowId(after.Handle)
            }
        };
    }

    private Observation BuildObservation(ActiveWindow window, JsonElement args, bool inspect, CancellationToken token,
        CaptureDisplay? display = null, bool includeDisplays = false)
    {
        if (approvedEditor != (window.Handle, window.ProcessId) && window.Handle == GetForegroundWindow())
        {
            approvedEditor = null;
        }
        var items = new List<object>();
        var marks = new List<ScreenMark>();
        var screen = display?.Bounds ?? WindowsInput.VirtualScreen;
        if (inspect)
        {
            Invalidate();
            inspectedWindow = window.Handle;
            inspectedAt = DateTimeOffset.UtcNow;
            int depthLimit = Math.Clamp(Number(args, "max_depth", 6), 1, 12);
            var interactiveOnly = !args.TryGetProperty("interactive_only", out var only) || only.GetBoolean();
            var filter = String(args, "filter_text").Trim();
            var pending = new Queue<(AutomationElement Element, int Depth)>();
            var walker = TreeWalker.ControlViewWalker;
            pending.Enqueue((window.Root, 0));
            var visited = 0;
            while (pending.Count > 0 && items.Count < MaxElements && visited++ < (filter.Length == 0 ? 500 : 1500))
            {
                token.ThrowIfCancellationRequested();
                var (element, depth) = pending.Dequeue();
                try
                {
                    var info = element.Current;
                    var bounds = info.BoundingRectangle;
                    var kind = info.ControlType;
                    var role = kind.ProgrammaticName.Replace("ControlType.", "").ToLowerInvariant();
                    if (depth > 0 && !info.IsOffscreen && !info.IsPassword && info.IsEnabled &&
                        !bounds.IsEmpty && bounds.IntersectsWith(screen) &&
                        (!interactiveOnly || IsInteractive(kind, info.IsKeyboardFocusable)))
                    {
                        var value = element.TryGetCurrentPattern(ValuePattern.Pattern, out var pattern) &&
                            pattern is ValuePattern editable ? Trim(editable.Current.Value, 120) : "";
                        if (filter.Length == 0 || info.Name.Contains(filter, StringComparison.OrdinalIgnoreCase) ||
                            role.Contains(filter, StringComparison.OrdinalIgnoreCase) || value.Contains(filter, StringComparison.OrdinalIgnoreCase))
                        {
                            // Keep IDs distinct across inspections so stale IDs cannot select new elements.
                            var id = nextElementId;
                            nextElementId = nextElementId == int.MaxValue ? 1 : nextElementId + 1;
                            var region = ScreenRegion.FromRect(Rect.Intersect(bounds, screen));
                            elements[id] = element;
                            items.Add(new
                            {
                                id, role, name = Trim(info.Name, 64), value, bounds = region.ToArray(),
                                automation_id = Trim(info.AutomationId, 80), focused = info.HasKeyboardFocus
                            });
                            marks.Add(new ScreenMark(id, region with { X = region.X - (int)screen.X, Y = region.Y - (int)screen.Y }));
                        }
                    }
                    if (depth >= depthLimit)
                    {
                        continue;
                    }
                    int siblings = 0;
                    for (var child = walker.GetFirstChild(element); child is not null && pending.Count < 500 && siblings++ < 150;
                        child = walker.GetNextSibling(child))
                    {
                        pending.Enqueue((child, depth + 1));
                    }
                }
                catch (ElementNotAvailableException)
                {
                    // UI Automation providers can remove a node during traversal.
                }
            }
        }
        var regionOfWindow = ScreenRegion.FromRect(Rect.Intersect(window.Bounds, screen));
        var data = new
        {
            active_app = window.App, window_title = window.Title, screen_size = new[] { (int)screen.Width, (int)screen.Height },
            screen_origin = new[] { (int)screen.X, (int)screen.Y }, monitor = display?.Index,
            capture_bounds = display is null ? null : (Flag(args, "window_only") ? regionOfWindow : ScreenRegion.FromRect(screen)).ToArray(),
            window_id = WindowId(window.Handle),
            window_bounds = ScreenRegion.FromRect(window.Bounds).ToArray(), elements = items,
            helper_privilege = includeDisplays ? HelperPrivilege() : null,
            displays = includeDisplays ? DesktopCapture.Displays().Select(item => new
            {
                monitor = item.Index, primary = item.Primary, bounds = ScreenRegion.FromRect(item.Bounds).ToArray()
            }).ToArray() : null
        };
        return new Observation(data, marks, regionOfWindow with { X = regionOfWindow.X - (int)screen.X, Y = regionOfWindow.Y - (int)screen.Y });
    }

    private static string HelperPrivilege()
    {
        using var identity = WindowsIdentity.GetCurrent();
        if (identity.IsSystem)
        {
            return "system";
        }
        return new WindowsPrincipal(identity).IsInRole(WindowsBuiltInRole.Administrator) ? "elevated" : "standard";
    }

    private bool TryRoutineClick(ActiveWindow window, JsonElement args, CancellationToken token, out int[]? clickedXY)
    {
        clickedXY = null;
        var btn = String(args, "button");
        if (btn is "right" or "middle" || Flag(args, "double") || !args.TryGetProperty("element_id", out var id))
        {
            return false;
        }
        var element = Find(window, id.GetInt32());
        var info = element.Current;
        if (IsEditor(window, element) && info.IsKeyboardFocusable)
        {
            ForceForeground(window.Handle);
            token.ThrowIfCancellationRequested();
            element.SetFocus();
            return true;
        }
        if (info.ControlType == ControlType.MenuItem &&
            element.TryGetCurrentPattern(ExpandCollapsePattern.Pattern, out var pattern) &&
            pattern is ExpandCollapsePattern expandable)
        {
            if (expandable.Current.ExpandCollapseState is ExpandCollapseState.Collapsed or ExpandCollapseState.PartiallyExpanded)
            {
                ForceForeground(window.Handle);
                token.ThrowIfCancellationRequested();
                expandable.Expand();
                return true;
            }
        }
        if (window.TrustedCalculator && info.ControlType == ControlType.Button && IsCalculatorButton(info.AutomationId) &&
            element.TryGetCurrentPattern(InvokePattern.Pattern, out var buttonPattern) && buttonPattern is InvokePattern button)
        {
            ForceForeground(window.Handle);
            token.ThrowIfCancellationRequested();
            button.Invoke();
            return true;
        }
        return false;
    }

    private static bool IsCalculatorButton(string id) => CalculatorButtons.Contains(id) ||
        (id.Length == 10 && id.StartsWith("num", StringComparison.Ordinal) &&
            id[3] is >= '0' and <= '9' && id.EndsWith("Button", StringComparison.Ordinal));

    private static bool IsRoutineKey(ActiveWindow window, string[] keys)
    {
        var focused = AutomationElement.FocusedElement;
        if (keys.Length != 1 || focused is null || focused.Current.ProcessId != window.ProcessId ||
            focused.Current.IsPassword)
        {
            return false;
        }
        return NavigationKeys.Contains(keys[0]) ||
            (window.TrustedNotepad && keys[0].Equals("Enter", StringComparison.OrdinalIgnoreCase) && IsEditor(window, focused));
    }

    private int[]? Click(ActiveWindow window, JsonElement args, CancellationToken token)
    {
        var button = String(args, "button");
        if (string.IsNullOrEmpty(button)) button = "left";
        var twice = Flag(args, "double");
        var hoverOnly = Flag(args, "hover_only");
        string[]? modifiers = args.TryGetProperty("modifiers", out var mods) && mods.ValueKind == JsonValueKind.Array
            ? mods.EnumerateArray().Select(m => m.GetString() ?? "").Where(m => m.Length > 0).ToArray()
            : null;

        if (args.TryGetProperty("element_id", out var id))
        {
            var element = Find(window, id.GetInt32());
            ForceForeground(window.Handle);
            if (!hoverOnly && button == "left" && !twice && (modifiers is null || modifiers.Length == 0) &&
                element.TryGetCurrentPattern(InvokePattern.Pattern, out var pattern) && pattern is InvokePattern invoke)
            {
                token.ThrowIfCancellationRequested();
                invoke.Invoke();
                var r = element.Current.BoundingRectangle;
                return r.IsEmpty ? null : [(int)(r.Left + r.Width / 2), (int)(r.Top + r.Height / 2)];
            }
            var bounds = element.Current.BoundingRectangle;
            var x = (int)(bounds.Left + bounds.Width / 2);
            var y = (int)(bounds.Top + bounds.Height / 2);
            EnsurePoint(x, y);
            WindowsInput.Click(x, y, button, twice, hoverOnly, modifiers, token);
            return [x, y];
        }
        var pointX = Number(args, "x", -1);
        var pointY = Number(args, "y", -1);
        EnsurePoint(pointX, pointY);
        WindowsInput.Click(pointX, pointY, button, twice, hoverOnly, modifiers, token);
        return [pointX, pointY];
    }

    private AutomationElement? TryResolveEditor(ActiveWindow window, JsonElement args)
    {
        if (args.TryGetProperty("element_id", out var id))
        {
            var element = Find(window, id.GetInt32());
            if (element.Current.IsPassword)
            {
                throw new CaptureException("E_UI_EDIT_REQUIRED");
            }
            return element;
        }
        try
        {
            var focused = AutomationElement.FocusedElement;
            if (focused is not null && focused.Current.IsPassword)
            {
                throw new CaptureException("E_UI_EDIT_REQUIRED");
            }
            return focused;
        }
        catch (ElementNotAvailableException)
        {
            return null;
        }
    }

    private static bool IsEditor(ActiveWindow window, AutomationElement element)
    {
        var info = element.Current;
        return info.ProcessId == window.ProcessId && info.IsEnabled && !info.IsOffscreen && !info.IsPassword &&
            (info.ControlType == ControlType.Edit || info.ControlType == ControlType.Document);
    }

    private static void EnterText(ActiveWindow window, AutomationElement? element, string text, string mode,
        bool clear, bool submit, CancellationToken token)
    {
        if (element is not null && mode is not ("paste" or "type") &&
            element.TryGetCurrentPattern(ValuePattern.Pattern, out var pattern) && pattern is ValuePattern value && !value.Current.IsReadOnly)
        {
            ForceForeground(window.Handle);
            token.ThrowIfCancellationRequested();
            value.SetValue(clear ? text : value.Current.Value + text);
        }
        else
        {
            if (element is not null)
            {
                try
                {
                    element.SetFocus();
                }
                catch
                {
                    try
                    {
                        var bounds = element.Current.BoundingRectangle;
                        if (!bounds.IsEmpty)
                        {
                            var cx = (int)(bounds.Left + bounds.Width / 2);
                            var cy = (int)(bounds.Top + bounds.Height / 2);
                            if (WindowsInput.VirtualScreen.Contains(cx, cy))
                            {
                                WindowsInput.Click(cx, cy, "left", false, false, null, token);
                            }
                        }
                    }
                    catch
                    {
                    }
                }
            }
            if (clear)
            {
                WindowsInput.Keys(["Ctrl", "A"], token);
            }
            var usePaste = mode == "paste" || (mode != "type" && (text.Length > 80 || text.Contains('\n')));
            if (usePaste)
            {
                PasteViaClipboard(text, token);
            }
            else
            {
                WindowsInput.Text(text, token);
            }
        }
        if (submit)
        {
            WindowsInput.Keys(["Enter"], token);
        }
    }

    private static void PasteViaClipboard(string text, CancellationToken token)
    {
        string? previous = null;
        var hadText = false;
        Application.Current.Dispatcher.Invoke(() =>
        {
            try
            {
                if (Clipboard.ContainsText())
                {
                    previous = Clipboard.GetText();
                    hadText = true;
                }
                Clipboard.SetText(text);
            }
            catch
            {
                // Retry once after brief yield if another process held clipboard open.
                Thread.Sleep(30);
                Clipboard.SetText(text);
            }
        });
        if (token.WaitHandle.WaitOne(25))
        {
            token.ThrowIfCancellationRequested();
        }
        WindowsInput.Keys(["Ctrl", "V"], token);
        if (hadText && previous is not null)
        {
            if (!token.WaitHandle.WaitOne(90))
            {
                Application.Current.Dispatcher.Invoke(() =>
                {
                    try { Clipboard.SetText(previous); } catch { }
                });
            }
        }
    }

    private static void Scroll(ActiveWindow window, JsonElement args, CancellationToken token)
    {
        if (args.TryGetProperty("from_xy", out var from) && args.TryGetProperty("to_xy", out var to))
        {
            int x = from[0].GetInt32(), y = from[1].GetInt32(), endX = to[0].GetInt32(), endY = to[1].GetInt32();
            EnsurePoint(x, y);
            EnsurePoint(endX, endY);
            WindowsInput.Drag(x, y, endX, endY, token);
            return;
        }
        int targetX, targetY;
        if (args.TryGetProperty("x", out var xProp) && args.TryGetProperty("y", out var yProp))
        {
            targetX = xProp.GetInt32();
            targetY = yProp.GetInt32();
        }
        else
        {
            var region = ScreenRegion.FromRect(Rect.Intersect(window.Bounds, WindowsInput.VirtualScreen));
            targetX = region.X + region.Width / 2;
            targetY = region.Y + region.Height / 2;
        }
        EnsurePoint(targetX, targetY);
        WindowsInput.Scroll(targetX, targetY, String(args, "direction"), Number(args, "distance", 0), token);
    }

    private object Launch(JsonElement args, bool approved, CancellationToken token)
    {
        var choice = String(args, "name_or_path").Trim();
        var launchArgs = String(args, "args");
        var cwd = Environment.ExpandEnvironmentVariables(String(args, "cwd").Trim());
        var runAsAdmin = Flag(args, "run_as_admin");

        if (string.IsNullOrEmpty(choice))
        {
            throw new CaptureException("E_APP_NOT_FOUND");
        }

        var normalized = choice.ToLowerInvariant() switch
        {
            "notepad" or "记事本" => "notepad.exe",
            "calc" or "calculator" or "计算器" => "calc.exe",
            "mspaint" or "paint" or "画图" => "mspaint.exe",
            "task manager" or "taskmgr" or "任务管理器" => "taskmgr.exe",
            "file explorer" or "explorer" or "资源管理器" or "文件资源管理器" => "explorer.exe",
            "control panel" or "control" or "控制面板" => "control.exe",
            "regedit" or "注册表" or "注册表编辑器" => "regedit.exe",
            "terminal" or "windows terminal" or "wt" or "终端" => "wt.exe",
            "powershell" => "powershell.exe",
            "pwsh" => "pwsh.exe",
            "cmd" or "命令提示符" => "cmd.exe",
            "settings" or "设置" => "ms-settings:",
            "device manager" or "devmgmt" or "设备管理器" => "devmgmt.msc",
            "services" or "服务" => "services.msc",
            "mmc" or "计算机管理" => "compmgmt.msc",
            _ => Environment.ExpandEnvironmentVariables(choice)
        };

        var builtIn = normalized is "notepad.exe" or "calc.exe" or "mspaint.exe";
        if (!builtIn && !approved)
        {
            throw new CaptureException("E_CONFIRM_REQUIRED");
        }

        var target = ResolveLaunchTarget(normalized);
        if (target is null && normalized.Equals("wt.exe", StringComparison.OrdinalIgnoreCase))
        {
            target = ResolveLaunchTarget("powershell.exe");
        }
        if (target is null)
        {
            throw new CaptureException("E_APP_NOT_FOUND");
        }

        var psi = new ProcessStartInfo(target)
        {
            UseShellExecute = true,
            Arguments = launchArgs,
        };
        if (!string.IsNullOrEmpty(cwd) && Directory.Exists(cwd))
        {
            psi.WorkingDirectory = cwd;
        }
        if (runAsAdmin)
        {
            psi.Verb = "runas";
        }

        var previous = GetForegroundWindow();
        token.ThrowIfCancellationRequested();
        using var process = Process.Start(psi);
        approvedEditor = null;
        Invalidate();

        var pid = 0;
        var foreground = IntPtr.Zero;
        if (process is not null)
        {
            pid = process.Id;
            var exeName = Path.GetFileName(target);
            foreground = WaitForAppWindow(process, previous, exeName, target, token);
        }
        else
        {
            var wait = Stopwatch.StartNew();
            while (wait.ElapsedMilliseconds < 800)
            {
                token.ThrowIfCancellationRequested();
                var fg = GetForegroundWindow();
                if (fg != previous && fg != IntPtr.Zero)
                {
                    foreground = fg;
                    break;
                }
                Thread.Sleep(50);
            }
        }
        return new
        {
            data = new
            {
                status = foreground == IntPtr.Zero ? "window_pending" : "ok",
                application = Path.GetFileName(target) is { Length: > 0 } fn ? fn : target,
                pid,
                window_id = foreground == IntPtr.Zero ? "" : WindowId(foreground),
                window_title = foreground == IntPtr.Zero ? "" : NativeTitle(foreground)
            }
        };
    }

    private static string? ResolveLaunchTarget(string candidate)
    {
        if (candidate.Contains("://", StringComparison.Ordinal) ||
            candidate.StartsWith("ms-settings:", StringComparison.OrdinalIgnoreCase) ||
            candidate.StartsWith("shell:", StringComparison.OrdinalIgnoreCase))
        {
            return candidate;
        }

        if (Path.IsPathFullyQualified(candidate))
        {
            if (File.Exists(candidate) || Directory.Exists(candidate))
            {
                return candidate;
            }
            if (File.Exists(candidate + ".exe"))
            {
                return candidate + ".exe";
            }
            return null;
        }

        var namesToTry = Path.HasExtension(candidate)
            ? new[] { candidate }
            : new[] { candidate + ".exe", candidate + ".cmd", candidate + ".bat", candidate + ".msc", candidate };

        var searchDirs = new List<string>
        {
            Environment.SystemDirectory,
            Environment.GetFolderPath(Environment.SpecialFolder.SystemX86),
            Environment.GetFolderPath(Environment.SpecialFolder.Windows),
            Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData), "Microsoft", "WindowsApps"),
        };
        var pathEnv = Environment.GetEnvironmentVariable("PATH");
        if (!string.IsNullOrEmpty(pathEnv))
        {
            searchDirs.AddRange(pathEnv.Split(';', StringSplitOptions.RemoveEmptyEntries | StringSplitOptions.TrimEntries));
        }

        foreach (var dir in searchDirs)
        {
            if (string.IsNullOrEmpty(dir) || !Directory.Exists(dir)) continue;
            foreach (var n in namesToTry)
            {
                var full = Path.Combine(dir, n);
                if (File.Exists(full)) return full;
            }
        }

        foreach (var n in namesToTry)
        {
            var key = @"Software\Microsoft\Windows\CurrentVersion\App Paths\" + n;
            using var cu = Registry.CurrentUser.OpenSubKey(key);
            using var lm = Registry.LocalMachine.OpenSubKey(key);
            var val = (cu?.GetValue(null) as string ?? lm?.GetValue(null) as string)?.Trim('"');
            if (!string.IsNullOrEmpty(val) && File.Exists(val))
            {
                return val;
            }
        }

        var shortcutDirs = new[]
        {
            Environment.GetFolderPath(Environment.SpecialFolder.CommonStartMenu),
            Environment.GetFolderPath(Environment.SpecialFolder.StartMenu),
            Environment.GetFolderPath(Environment.SpecialFolder.DesktopDirectory),
            Environment.GetFolderPath(Environment.SpecialFolder.CommonDesktopDirectory),
        };
        var baseNeedle = Path.GetFileNameWithoutExtension(candidate);
        string? fuzzyMatch = null;
        foreach (var root in shortcutDirs)
        {
            if (string.IsNullOrEmpty(root) || !Directory.Exists(root)) continue;
            try
            {
                foreach (var file in Directory.EnumerateFiles(root, "*.lnk", SearchOption.AllDirectories))
                {
                    var stem = Path.GetFileNameWithoutExtension(file);
                    if (stem.Equals(baseNeedle, StringComparison.OrdinalIgnoreCase) || stem.Equals(candidate, StringComparison.OrdinalIgnoreCase))
                    {
                        return file;
                    }
                    if (fuzzyMatch is null && stem.Contains(baseNeedle, StringComparison.OrdinalIgnoreCase))
                    {
                        fuzzyMatch = file;
                    }
                }
            }
            catch
            {
            }
        }
        return fuzzyMatch;
    }

    private object OpenTerminal(JsonElement args, bool approved, CancellationToken token)
    {
        if (!approved)
        {
            throw new CaptureException("E_CONFIRM_REQUIRED");
        }
        var shell = String(args, "shell").ToLowerInvariant();
        if (string.IsNullOrEmpty(shell)) shell = "powershell";
        var command = String(args, "command");
        var cwd = Environment.ExpandEnvironmentVariables(String(args, "cwd").Trim());
        var title = String(args, "title");

        string exe;
        string cliArgs = "";
        switch (shell)
        {
            case "cmd":
                exe = ResolveLaunchTarget("cmd.exe") ?? "cmd.exe";
                if (!string.IsNullOrEmpty(title) || !string.IsNullOrEmpty(command))
                {
                    var parts = new List<string>();
                    if (!string.IsNullOrEmpty(title)) parts.Add("title " + title.Replace("\"", ""));
                    if (!string.IsNullOrEmpty(command)) parts.Add(command);
                    cliArgs = "/K " + string.Join(" && ", parts);
                }
                break;
            case "pwsh":
                exe = ResolveLaunchTarget("pwsh.exe") ?? ResolveLaunchTarget("powershell.exe") ?? "powershell.exe";
                cliArgs = BuildInteractivePSArgs(command, title);
                break;
            case "wt":
                exe = ResolveLaunchTarget("wt.exe") ?? "";
                if (string.IsNullOrEmpty(exe))
                {
                    exe = ResolveLaunchTarget("powershell.exe") ?? "powershell.exe";
                    cliArgs = BuildInteractivePSArgs(command, title);
                }
                else
                {
                    var wtParts = new List<string>();
                    if (!string.IsNullOrEmpty(cwd) && Directory.Exists(cwd)) wtParts.Add($"-d \"{cwd}\"");
                    if (!string.IsNullOrEmpty(title)) wtParts.Add($"--title \"{title.Replace("\"", "")}\"");
                    if (!string.IsNullOrEmpty(command))
                    {
                        wtParts.Add("powershell.exe " + BuildInteractivePSArgs(command, title));
                    }
                    cliArgs = string.Join(" ", wtParts);
                }
                break;
            default:
                exe = ResolveLaunchTarget("powershell.exe") ?? "powershell.exe";
                cliArgs = BuildInteractivePSArgs(command, title);
                break;
        }

        var psi = new ProcessStartInfo(exe)
        {
            UseShellExecute = true,
            Arguments = cliArgs,
        };
        if (!string.IsNullOrEmpty(cwd) && Directory.Exists(cwd))
        {
            psi.WorkingDirectory = cwd;
        }

        var previous = GetForegroundWindow();
        token.ThrowIfCancellationRequested();
        using var process = Process.Start(psi) ?? throw new CaptureException("E_APP_NOT_STARTED");
        Invalidate();
        var hwnd = WaitForAppWindow(process, previous, Path.GetFileName(exe), exe, token);
        if (hwnd == IntPtr.Zero) hwnd = GetForegroundWindow();
        return new
        {
            data = new
            {
                status = "ok",
                pid = process.Id,
                window_id = hwnd == IntPtr.Zero ? "" : WindowId(hwnd),
                window_title = hwnd == IntPtr.Zero ? "" : NativeTitle(hwnd)
            }
        };
    }

    private static string BuildInteractivePSArgs(string command, string title)
    {
        var prefix = "";
        if (!string.IsNullOrEmpty(title))
        {
            prefix += $"$Host.UI.RawUI.WindowTitle = '{title.Replace("'", "''")}'; ";
        }
        var full = prefix + command;
        if (string.IsNullOrWhiteSpace(full))
        {
            return "-NoExit";
        }
        return $"-NoExit -ExecutionPolicy Bypass -Command \"{full.Replace("\"", "\\\"")}\"";
    }

    private static object ManageClipboard(JsonElement args, bool approved)
    {
        var action = String(args, "action");
        if (action is "set" or "clear" && !approved)
        {
            throw new CaptureException("E_CONFIRM_REQUIRED");
        }
        object result = Application.Current.Dispatcher.Invoke<object>(() =>
        {
            switch (action)
            {
                case "get":
                    var text = Clipboard.ContainsText() ? Clipboard.GetText() : "";
                    return new { status = "ok", text, length = text.Length };
                case "set":
                    var content = String(args, "text");
                    Clipboard.SetText(content);
                    return new { status = "ok", length = content.Length };
                case "clear":
                    Clipboard.Clear();
                    return new { status = "ok" };
                default:
                    throw new CaptureException("E_CAP_UNSUPPORTED");
            }
        });
        return new { data = result };
    }

    private static object PowerScreen(JsonElement args, bool approved)
    {
        var action = String(args, "action");
        if (action is "screen_off" or "lock_workstation" && !approved)
        {
            throw new CaptureException("E_CONFIRM_REQUIRED");
        }
        switch (action)
        {
            case "wake":
                WindowsInput.WakeMouse();
                PostMessage(new IntPtr(0xFFFF), 0x0112, new IntPtr(0xF170), new IntPtr(-1));
                return new { data = new { status = "ok", action } };
            case "screen_off":
                PostMessage(new IntPtr(0xFFFF), 0x0112, new IntPtr(0xF170), new IntPtr(2));
                return new { data = new { status = "ok", action } };
            case "lock_workstation":
                if (!LockWorkStation())
                {
                    throw new CaptureException("E_WINDOW_CONTROL_FAILED");
                }
                return new { data = new { status = "ok", action } };
            case "keep_awake":
                SetThreadExecutionState(0x80000003);
                return new { data = new { status = "ok", action } };
            default:
                throw new CaptureException("E_CAP_UNSUPPORTED");
        }
    }

    private static IntPtr WaitForAppWindow(Process process, IntPtr previous, string name, string path, CancellationToken token)
    {
        var pid = (uint)process.Id;
        try
        {
            process.WaitForInputIdle(1500);
        }
        catch (InvalidOperationException)
        {
        }
        catch (System.ComponentModel.Win32Exception)
        {
        }

        var executable = Path.GetFileNameWithoutExtension(path);
        var wait = Stopwatch.StartNew();
        while (wait.ElapsedMilliseconds < 1500)
        {
            token.ThrowIfCancellationRequested();
            var foreground = GetForegroundWindow();
            if (foreground != previous && IsAppWindow(foreground, pid, name, executable))
            {
                return foreground;
            }
            if (token.WaitHandle.WaitOne(50))
            {
                token.ThrowIfCancellationRequested();
            }
        }
        token.ThrowIfCancellationRequested();
        var current = GetForegroundWindow();
        return IsAppWindow(current, pid, name, executable) ? current : IntPtr.Zero;
    }

    private static bool IsAppWindow(IntPtr window, uint launchedPid, string name, string executable)
    {
        if (window == IntPtr.Zero) return false;
        GetWindowThreadProcessId(window, out var pid);
        if (pid == 0) return false;
        if (pid == launchedPid) return true;
        try
        {
            using var owner = Process.GetProcessById((int)pid);
            var processName = owner.ProcessName;
            return processName.Equals(executable, StringComparison.OrdinalIgnoreCase) ||
                (name == "calc.exe" && processName.Equals("CalculatorApp", StringComparison.OrdinalIgnoreCase));
        }
        catch
        {
            return false;
        }
    }

    private void Invalidate()
    {
        elements.Clear();
        inspectedWindow = IntPtr.Zero;
    }

    private AutomationElement Find(ActiveWindow window, int id)
    {
        if (inspectedWindow != window.Handle || DateTimeOffset.UtcNow - inspectedAt > TimeSpan.FromMinutes(5) ||
            !elements.TryGetValue(id, out var element))
        {
            throw new CaptureException("E_UI_INSPECT_AGAIN");
        }
        try
        {
            var info = element.Current;
            if (info.IsEnabled && !info.IsOffscreen && !info.IsPassword && info.ProcessId == window.ProcessId)
            {
                return element;
            }
        }
        catch (ElementNotAvailableException)
        {
        }
        throw new CaptureException("E_UI_INSPECT_AGAIN");
    }

    private static ActiveWindow Foreground() => ResolveWindow("");

    private static ActiveWindow? TryForeground()
    {
        try { return Foreground(); } catch { return null; }
    }

    private static ActiveWindow ResolveWindow(string id)
    {
        var handle = id switch
        {
            "" => GetForegroundWindow(),
            "desktop" => GetShellWindow(),
            "taskbar" => FindWindow("Shell_TrayWnd", null),
            _ when long.TryParse(id, NumberStyles.None, CultureInfo.InvariantCulture, out var value) => new IntPtr(value),
            _ => IntPtr.Zero,
        };
        if (handle == IntPtr.Zero || !IsWindowVisible(handle) ||
            GetAncestor(handle, 2) != handle || !GetWindowRect(handle, out var bounds))
        {
            throw new CaptureException("E_UI_NOT_FOUND");
        }
        var root = AutomationElement.FromHandle(handle);
        using var process = Process.GetProcessById(root.Current.ProcessId);
        var app = process.ProcessName.ToLowerInvariant();
        var mainWindow = GetWindow(handle, OwnerWindow) == IntPtr.Zero;
        return new ActiveWindow(handle, root, process.Id, app,
            mainWindow && app == "notepad" && IsWindowsApp(process, "notepad.exe", "Microsoft.WindowsNotepad_", "notepad.exe"),
            mainWindow && (app is "calc" or "calculatorapp") &&
                IsWindowsApp(process, "calc.exe", "Microsoft.WindowsCalculator_", "CalculatorApp.exe"),
            Trim(root.Current.Name, 120),
            new Rect(bounds.Left, bounds.Top, Math.Max(0, bounds.Right - bounds.Left), Math.Max(0, bounds.Bottom - bounds.Top)));
    }

    private static bool IsWindowsApp(Process process, string systemExecutable, string packagePrefix, string packageExecutable)
    {
        try
        {
            var path = process.MainModule?.FileName;
            if (path is null) return false;
            var systemPath = Path.Combine(Environment.SystemDirectory, systemExecutable);
            var packagePath = Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ProgramFiles), "WindowsApps", packagePrefix);
            return path.Equals(systemPath, StringComparison.OrdinalIgnoreCase) ||
                (path.StartsWith(packagePath, StringComparison.OrdinalIgnoreCase) &&
                    Path.GetFileName(path).Equals(packageExecutable, StringComparison.OrdinalIgnoreCase));
        }
        catch (System.ComponentModel.Win32Exception)
        {
            return false;
        }
    }

    private static void EnsurePoint(int x, int y)
    {
        if (!WindowsInput.VirtualScreen.Contains(x, y))
        {
            throw new CaptureException("E_UI_POINT_OUTSIDE_WINDOW");
        }
    }

    internal static bool ForceForeground(IntPtr window)
    {
        if (GetForegroundWindow() == window) return true;
        if (IsIconic(window))
        {
            ShowWindowAsync(window, 9);
        }
        if (SetForegroundWindow(window)) return true;
        var fg = GetForegroundWindow();
        var fgThread = fg == IntPtr.Zero ? 0 : GetWindowThreadProcessId(fg, out _);
        var targetThread = GetWindowThreadProcessId(window, out _);
        var curThread = GetCurrentThreadId();
        var attachedFg = fgThread != 0 && fgThread != curThread && AttachThreadInput(curThread, fgThread, true);
        var attachedTarget = targetThread != 0 && targetThread != curThread && AttachThreadInput(curThread, targetThread, true);
        try
        {
            BringWindowToTop(window);
            ShowWindowAsync(window, IsIconic(window) ? 9 : 5);
            return SetForegroundWindow(window) || GetForegroundWindow() == window;
        }
        finally
        {
            if (attachedTarget) AttachThreadInput(curThread, targetThread, false);
            if (attachedFg) AttachThreadInput(curThread, fgThread, false);
        }
    }

    private static object ListWindows(JsonElement args, CancellationToken token)
    {
        var foreground = GetForegroundWindow();
        var filter = String(args, "filter").Trim();
        var includeMinimized = !args.TryGetProperty("include_minimized", out var minimized) || minimized.GetBoolean();
        var windows = new List<object>();
        EnumWindows((handle, _) =>
        {
            if (token.IsCancellationRequested || windows.Count >= 160) return false;
            if (!IsWindowVisible(handle) || !GetWindowRect(handle, out var bounds) ||
                GetWindowThreadProcessId(handle, out var pid) == 0 || pid == (uint)Environment.ProcessId)
            {
                return true;
            }
            var isMinimized = IsIconic(handle);
            if (!includeMinimized && isMinimized) return true;
            var title = NativeTitle(handle);
            if (title.Length == 0) return true;
            string app;
            try
            {
                using var process = Process.GetProcessById((int)pid);
                app = process.ProcessName;
            }
            catch
            {
                app = "";
            }
            if (title.Contains(filter, StringComparison.OrdinalIgnoreCase) || app.Contains(filter, StringComparison.OrdinalIgnoreCase))
            {
                windows.Add(new
                {
                    window_id = WindowId(handle), title, process_name = app, pid,
                    bounds = new[] { bounds.Left, bounds.Top, bounds.Right - bounds.Left, bounds.Bottom - bounds.Top },
                    state = isMinimized ? "minimized" : IsZoomed(handle) ? "maximized" : "normal",
                    is_foreground = foreground == handle
                });
            }
            return true;
        }, IntPtr.Zero);
        token.ThrowIfCancellationRequested();
        return new { data = new { foreground_window_id = WindowId(foreground), windows } };
    }

    private object ControlWindow(JsonElement args, bool approved, CancellationToken token)
    {
        IntPtr window = IntPtr.Zero;
        if (long.TryParse(String(args, "window_id"), NumberStyles.None, CultureInfo.InvariantCulture, out var rawHandle))
        {
            window = new IntPtr(rawHandle);
        }
        else
        {
            var titleFilter = String(args, "title_contains").Trim();
            var procFilter = String(args, "process_name").Trim();
            if (titleFilter.Length > 0 || procFilter.Length > 0)
            {
                EnumWindows((handle, _) =>
                {
                    if (!IsWindowVisible(handle) || GetAncestor(handle, 2) != handle ||
                        GetWindowThreadProcessId(handle, out var wpid) == 0 || wpid == (uint)Environment.ProcessId)
                    {
                        return true;
                    }
                    var t = NativeTitle(handle);
                    if (t.Length == 0) return true;
                    if (titleFilter.Length > 0 && !t.Contains(titleFilter, StringComparison.OrdinalIgnoreCase)) return true;
                    if (procFilter.Length > 0)
                    {
                        try
                        {
                            using var p = Process.GetProcessById((int)wpid);
                            if (!p.ProcessName.Contains(procFilter, StringComparison.OrdinalIgnoreCase)) return true;
                        }
                        catch
                        {
                            return true;
                        }
                    }
                    window = handle;
                    return false;
                }, IntPtr.Zero);
            }
        }

        if (window == IntPtr.Zero || !IsWindowVisible(window) || GetAncestor(window, 2) != window ||
            GetWindowThreadProcessId(window, out var pid) == 0 || pid == (uint)Environment.ProcessId)
        {
            throw new CaptureException("E_UI_NOT_FOUND");
        }
        if (!approved)
        {
            throw new CaptureException("E_CONFIRM_REQUIRED");
        }
        if (!DesktopCapture.IsDefaultDesktop())
        {
            throw new CaptureException("E_DESKTOP_LOCKED");
        }
        var title = NativeTitle(window);
        token.ThrowIfCancellationRequested();
        var action = String(args, "action");
        bool changed;
        switch (action)
        {
            case "focus":
                changed = ForceForeground(window);
                break;
            case "minimize":
                changed = ShowWindowAsync(window, 6);
                break;
            case "maximize":
                changed = ShowWindowAsync(window, 3);
                break;
            case "restore":
                changed = ShowWindowAsync(window, 9);
                break;
            case "close":
                changed = PostMessage(window, 0x0010, IntPtr.Zero, IntPtr.Zero);
                break;
            case "move_resize":
                var bounds = args.GetProperty("bounds");
                var rectangle = new Rect(bounds[0].GetInt32(), bounds[1].GetInt32(), bounds[2].GetInt32(), bounds[3].GetInt32());
                if (!WindowsInput.VirtualScreen.IntersectsWith(rectangle))
                {
                    throw new CaptureException("E_UI_POINT_OUTSIDE_WINDOW");
                }
                changed = SetWindowPos(window, IntPtr.Zero, (int)rectangle.X, (int)rectangle.Y,
                    (int)rectangle.Width, (int)rectangle.Height, 0x0014);
                break;
            default:
                throw new CaptureException("E_CAP_UNSUPPORTED");
        }
        if (!changed)
        {
            throw new CaptureException("E_WINDOW_CONTROL_FAILED");
        }
        Invalidate();
        approvedEditor = null;
        return new { data = new { status = "ok", window_id = WindowId(window), window_title = title, action } };
    }

    private static object WaitFor(JsonElement args, CancellationToken token)
    {
        var title = String(args, "window_title_contains").Trim();
        var text = String(args, "text_contains").Trim();
        var role = String(args, "role").Trim();
        var disappear = String(args, "state") == "disappear";
        var timeout = Number(args, "timeout_ms", 5000);
        var watch = Stopwatch.StartNew();
        do
        {
            token.ThrowIfCancellationRequested();
            if (!DesktopCapture.IsDefaultDesktop())
            {
                throw new CaptureException("E_DESKTOP_LOCKED");
            }
            var candidates = new List<IntPtr>();
            if (title.Length == 0)
            {
                candidates.Add(GetForegroundWindow());
            }
            else
            {
                EnumWindows((window, _) =>
                {
                    if (token.IsCancellationRequested || candidates.Count >= 40) return false;
                    if (IsWindowVisible(window) && NativeTitle(window).Contains(title, StringComparison.OrdinalIgnoreCase))
                    {
                        candidates.Add(window);
                    }
                    return true;
                }, IntPtr.Zero);
            }
            IntPtr matched = IntPtr.Zero;
            object? element = null;
            foreach (var window in candidates)
            {
                token.ThrowIfCancellationRequested();
                if (window == IntPtr.Zero) continue;
                if (text.Length == 0 && role.Length == 0 || (element = FindWaitElement(window, text, role, token)) is not null)
                {
                    matched = window;
                    break;
                }
                if (watch.ElapsedMilliseconds >= timeout) break;
            }
            if (disappear ? matched == IntPtr.Zero : matched != IntPtr.Zero)
            {
                return new { data = new { matched = true, elapsed_ms = watch.ElapsedMilliseconds,
                    window_id = matched == IntPtr.Zero ? null : WindowId(matched), element } };
            }
            var remaining = Math.Min(200, timeout - watch.ElapsedMilliseconds);
            if (remaining > 0 && token.WaitHandle.WaitOne((int)remaining))
            {
                token.ThrowIfCancellationRequested();
            }
        } while (watch.ElapsedMilliseconds < timeout);
        return new { data = new { matched = false, elapsed_ms = watch.ElapsedMilliseconds } };
    }

    private static object? FindWaitElement(IntPtr window, string text, string role, CancellationToken token)
    {
        var pending = new Queue<AutomationElement>();
        var walker = TreeWalker.ControlViewWalker;
        try
        {
            pending.Enqueue(AutomationElement.FromHandle(window));
            for (var visited = 0; pending.Count > 0 && visited < 500; visited++)
            {
                token.ThrowIfCancellationRequested();
                var element = pending.Dequeue();
                try
                {
                    var info = element.Current;
                    var currentRole = info.ControlType.ProgrammaticName.Replace("ControlType.", "").ToLowerInvariant();
                    if (!info.IsOffscreen && !info.IsPassword && info.Name.Contains(text, StringComparison.OrdinalIgnoreCase) &&
                        (role.Length == 0 || currentRole.Equals(role, StringComparison.OrdinalIgnoreCase)))
                    {
                        return new { role = currentRole, name = Trim(info.Name, 120), bounds = ScreenRegion.FromRect(info.BoundingRectangle).ToArray() };
                    }
                    for (var child = walker.GetFirstChild(element); child is not null && pending.Count < 500; child = walker.GetNextSibling(child))
                    {
                        pending.Enqueue(child);
                    }
                }
                catch (ElementNotAvailableException)
                {
                }
            }
        }
        catch (ElementNotAvailableException)
        {
        }
        return null;
    }

    private static string NativeTitle(IntPtr handle)
    {
        var buffer = new System.Text.StringBuilder(257);
        GetWindowText(handle, buffer, buffer.Capacity);
        return buffer.ToString();
    }

    private static bool IsInteractive(ControlType kind, bool focusable) => focusable || kind == ControlType.Button ||
        kind == ControlType.Edit || kind == ControlType.Document || kind == ControlType.Hyperlink ||
        kind == ControlType.MenuItem || kind == ControlType.ListItem || kind == ControlType.ComboBox ||
        kind == ControlType.CheckBox || kind == ControlType.TabItem || kind == ControlType.RadioButton;

    private static string Trim(string? text, int length) => string.IsNullOrEmpty(text) ? "" : text[..Math.Min(text.Length, length)];
    private static string WindowId(IntPtr window) => window.ToInt64().ToString(CultureInfo.InvariantCulture);
    private static string String(JsonElement args, string key) => args.TryGetProperty(key, out var value) ? value.GetString() ?? "" : "";
    private static bool Flag(JsonElement args, string key) => args.TryGetProperty(key, out var value) && value.GetBoolean();
    private static int Number(JsonElement args, string key, int fallback) => args.TryGetProperty(key, out var value) ? value.GetInt32() : fallback;
    private sealed record ActiveWindow(IntPtr Handle, AutomationElement Root, int ProcessId, string App,
        bool TrustedNotepad, bool TrustedCalculator, string Title, Rect Bounds);
    private sealed record Observation(object Data, IReadOnlyList<ScreenMark> Marks, ScreenRegion Region);
    [StructLayout(LayoutKind.Sequential)]
    private struct NativeRect { public int Left, Top, Right, Bottom; }
    [DllImport("user32.dll")] private static extern IntPtr GetForegroundWindow();
    [DllImport("user32.dll")] private static extern IntPtr GetWindow(IntPtr window, uint command);
    [DllImport("user32.dll")] private static extern uint GetWindowThreadProcessId(IntPtr window, out uint processId);
    [DllImport("user32.dll")] private static extern bool GetWindowRect(IntPtr window, out NativeRect rect);
    [DllImport("user32.dll")] private static extern IntPtr GetAncestor(IntPtr window, uint flags);
    [DllImport("user32.dll")] private static extern IntPtr SetThreadDpiAwarenessContext(IntPtr value);
    private delegate bool WindowCallback(IntPtr window, IntPtr parameter);
    [DllImport("user32.dll")] private static extern bool EnumWindows(WindowCallback callback, IntPtr parameter);
    [DllImport("user32.dll")] private static extern bool IsWindowVisible(IntPtr window);
    [DllImport("user32.dll")] private static extern bool IsIconic(IntPtr window);
    [DllImport("user32.dll")] private static extern bool IsZoomed(IntPtr window);
    [DllImport("user32.dll")] private static extern bool ShowWindowAsync(IntPtr window, int command);
    [DllImport("user32.dll")] private static extern bool SetForegroundWindow(IntPtr window);
    [DllImport("user32.dll")] private static extern bool BringWindowToTop(IntPtr window);
    [DllImport("user32.dll")] private static extern bool AttachThreadInput(uint idAttach, uint idAttachTo, bool fAttach);
    [DllImport("kernel32.dll")] private static extern uint GetCurrentThreadId();
    [DllImport("user32.dll")] private static extern bool LockWorkStation();
    [DllImport("kernel32.dll")] private static extern uint SetThreadExecutionState(uint esFlags);
    [DllImport("user32.dll")] private static extern bool PostMessage(IntPtr window, uint message, IntPtr wParam, IntPtr lParam);
    [DllImport("user32.dll")] private static extern bool SetWindowPos(IntPtr window, IntPtr insertAfter, int x, int y, int width, int height, uint flags);
    [DllImport("user32.dll")] private static extern IntPtr GetShellWindow();
    [DllImport("user32.dll", CharSet = CharSet.Unicode)] private static extern IntPtr FindWindow(string className, string? title);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)] private static extern int GetWindowText(IntPtr window, System.Text.StringBuilder title, int capacity);
}
