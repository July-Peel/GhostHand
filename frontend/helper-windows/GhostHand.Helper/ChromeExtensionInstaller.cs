using Microsoft.Win32;
using System.Diagnostics;
using System.IO;
using System.Runtime.InteropServices;
using System.Security.Principal;
using System.Windows.Automation;

namespace GhostHand.Helper;

internal static class ChromeExtensionInstaller
{
    private const string ExtensionName = "GhostHand Browser Bridge";
    private const string ExtensionId = "mfbgehccogcjllkhnmhdkhfmbfbgjjnh";
    private const string HostName = "com.ghosthand.nativemessage";
    private const uint WM_GETOBJECT = 0x003d, WM_SETTEXT = 0x000c, BM_CLICK = 0x00f5, WM_CLOSE = 0x0010;
    private const uint SMTO_ABORTIFHUNG = 2, GW_OWNER = 4;
    private const int IDOK = 1;
    private static readonly int DesktopSession = Process.GetCurrentProcess().SessionId;

    private sealed record Browser(string ProcessName, string Scheme, string RegistryPath, string InstallPath)
    {
        public string Page(string token) => $"{Scheme}://extensions/?ghosthand={token}";
    }

    public static object InstallUnpackedExtension(string name, string directory, string manifest, string token, CancellationToken cancellation)
    {
        var browser = Resolve(name);
        var root = Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.CommonApplicationData), "GhostHand", "chrome-extension");
        if (!Path.IsPathFullyQualified(directory) || !Path.IsPathFullyQualified(manifest) ||
            !string.Equals(Path.GetDirectoryName(Path.GetFullPath(directory)), root, StringComparison.OrdinalIgnoreCase) ||
            !string.Equals(Path.GetFullPath(manifest), Path.Combine(root, HostName + ".json"), StringComparison.OrdinalIgnoreCase) ||
            !File.Exists(Path.Combine(directory, "manifest.json")) || !File.Exists(manifest))
        {
            throw new CaptureException("E_BROWSER_EXTENSION_PATH");
        }

        using (var key = Registry.CurrentUser.CreateSubKey(browser.RegistryPath))
        {
            key.SetValue("", manifest, RegistryValueKind.String);
        }

        try
        {
            var window = OpenManagement(browser, token, cancellation);
            var load = FindLoadButton(window);
            if (load is null)
            {
                var control = Until(() => FindLoadButton(window) ?? FindDevModeToggle(window),
                    TimeSpan.FromSeconds(3), cancellation, "E_BROWSER_DEVELOPER_MODE_NOT_FOUND");
                load = FindLoadButton(window);
                if (load is null)
                {
                    if (control.TryGetCurrentPattern(TogglePattern.Pattern, out var pattern))
                    {
                        var state = (TogglePattern)pattern;
                        if (state.Current.ToggleState == ToggleState.Off) state.Toggle();
                    }
                    else
                    {
                        Invoke(control, cancellation);
                    }
                    load = Until(() => FindLoadButton(window), TimeSpan.FromSeconds(3), cancellation, "E_BROWSER_LOAD_UNPACKED_NOT_AVAILABLE");
                }
            }

            if (FindCard(window) is { } existing)
            {
                var enabled = FindCardControl(existing, "enableToggle", ["Enable", "启用", "啟用"]);
                if (enabled is not null && enabled.TryGetCurrentPattern(TogglePattern.Pattern, out var pattern) &&
                    ((TogglePattern)pattern).Current.ToggleState == ToggleState.Off)
                {
                    ((TogglePattern)pattern).Toggle();
                    return new { data = new { installed = true, already_loaded = true } };
                }
                var reload = FindCardControl(existing, "dev-reload-button", ["Reload", "重新加载", "重新載入", "刷新"]);
                if (reload is not null)
                {
                    Invoke(reload, cancellation);
                    return new { data = new { installed = true, already_loaded = true } };
                }
            }

            var originalDialogs = Dialogs(window).ToHashSet();
            var invoke = Task.Run(() => Invoke(load, cancellation), cancellation);
            IntPtr dialog = IntPtr.Zero;
            try
            {
                dialog = UntilHandle(() =>
                {
                    if (invoke.IsFaulted) invoke.GetAwaiter().GetResult();
                    return Dialogs(window).FirstOrDefault(handle => !originalDialogs.Contains(handle));
                }, TimeSpan.FromSeconds(5), cancellation, "E_BROWSER_FOLDER_DIALOG_NOT_FOUND");
                SetFolder(dialog, directory, cancellation);
            }
            finally
            {
                if (dialog != IntPtr.Zero && IsWindowVisible(dialog)) PostMessage(dialog, WM_CLOSE, IntPtr.Zero, IntPtr.Zero);
                Observe(invoke);
            }

            // The native handshake, rather than a UI label, confirms that Chrome activated the extension.
            return new { data = new { installed = true, already_loaded = false } };
        }
        catch
        {
            CloseManagement(browser, token, CancellationToken.None);
            throw;
        }
    }

    public static object RemoveExtensionViaUIA(string name, string token, CancellationToken cancellation)
    {
        var browser = Resolve(name);
        try
        {
            if (BrowserProcesses(browser).Count == 0)
            {
                return new { data = new { removed = false, browser_running = false } };
            }

            var window = OpenManagement(browser, token, cancellation);
            try
            {
                var card = FindCard(window);
                if (card is null) return new { data = new { removed = false, browser_running = true } };
                var remove = FindCardControl(card, "removeButton", ["Remove", "移除", "删除", "移除擴充功能"])
                    ?? throw new CaptureException("E_BROWSER_REMOVE_BUTTON_NOT_FOUND");
                var invoke = Task.Run(() => Invoke(remove, cancellation), cancellation);
                try
                {
                    var confirm = Until(() =>
                    {
                        if (invoke.IsFaulted) invoke.GetAwaiter().GetResult();
                        return Find(window, "remove-confirm", ["Remove extension", "移除扩展程序", "移除擴充功能"], preferButton: true)
                            ?? FindRemoveConfirmation(window, remove);
                    }, TimeSpan.FromSeconds(4), cancellation, "E_BROWSER_REMOVE_CONFIRM_NOT_FOUND");
                    Invoke(confirm, cancellation);
                    Until(() => FindCard(window) is null ? new object() : null,
                        TimeSpan.FromSeconds(3), cancellation, "E_BROWSER_REMOVE_FAILED");
                    return new { data = new { removed = true, browser_running = true } };
                }
                finally
                {
                    Observe(invoke);
                }
            }
            finally
            {
                CloseManagement(browser, token, CancellationToken.None);
            }
        }
        finally
        {
            Registry.CurrentUser.DeleteSubKey(browser.RegistryPath, throwOnMissingSubKey: false);
        }
    }

    public static object CloseManagementTab(string name, string token, CancellationToken cancellation)
    {
        return new { data = new { closed = CloseManagement(Resolve(name), token, cancellation) } };
    }

    private static Browser Resolve(string name) => name switch
    {
        "chrome" => new("chrome", "chrome", $@"Software\Google\Chrome\NativeMessagingHosts\{HostName}", @"Google\Chrome\Application\chrome.exe"),
        "edge" => new("msedge", "edge", $@"Software\Microsoft\Edge\NativeMessagingHosts\{HostName}", @"Microsoft\Edge\Application\msedge.exe"),
        _ => throw new CaptureException("E_BROWSER_UNSUPPORTED")
    };

    private static void ValidateToken(string token)
    {
        if (token.Length != 26 || token.Any(ch => !char.IsAsciiLetterUpper(ch) && !char.IsAsciiDigit(ch)))
            throw new CaptureException("E_BROWSER_INSTALL_TOKEN");
    }

    private static HashSet<uint> BrowserProcesses(Browser browser)
    {
        var ids = new HashSet<uint>();
        foreach (var process in Process.GetProcessesByName(browser.ProcessName))
        {
            using (process)
            {
                try
                {
                    if (process.SessionId == DesktopSession) ids.Add((uint)process.Id);
                }
                catch (InvalidOperationException)
                {
                    // Chromium may recycle a process during enumeration.
                }
            }
        }
        return ids;
    }

    private static string FindExecutable(Browser browser)
    {
        var appPath = @"SOFTWARE\Microsoft\Windows\CurrentVersion\App Paths\" + browser.ProcessName + ".exe";
        using var user = Registry.CurrentUser.OpenSubKey(appPath);
        using var machine = Registry.LocalMachine.OpenSubKey(appPath);
        var paths = new[]
        {
            machine?.GetValue(null) as string, user?.GetValue(null) as string,
            Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ProgramFiles), browser.InstallPath),
            Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.ProgramFilesX86), browser.InstallPath),
            Path.Combine(Environment.GetFolderPath(Environment.SpecialFolder.LocalApplicationData), browser.InstallPath)
        };
        foreach (var entry in paths)
        {
            var file = entry?.Trim('"') ?? "";
            if (Path.IsPathFullyQualified(file) && File.Exists(file) &&
                string.Equals(Path.GetFileName(file), browser.ProcessName + ".exe", StringComparison.OrdinalIgnoreCase))
                return file;
        }
        throw new CaptureException("E_BROWSER_NOT_INSTALLED");
    }

    private static void StartInDesktop(string exe, string arguments)
    {
        using var identity = WindowsIdentity.GetCurrent();
        if (!new WindowsPrincipal(identity).IsInRole(WindowsBuiltInRole.Administrator))
        {
            using var process = Process.Start(new ProcessStartInfo(exe, arguments) { UseShellExecute = false });
            return;
        }
        // Explorer executes in the desktop user's existing token and browser profile.
        var shellType = Type.GetTypeFromProgID("Shell.Application") ?? throw new CaptureException("E_BROWSER_DESKTOP_SHELL_UNAVAILABLE");
        dynamic shell = Activator.CreateInstance(shellType) ?? throw new CaptureException("E_BROWSER_DESKTOP_SHELL_UNAVAILABLE");
        object location = 0;
        object root = 0;
        try
        {
            dynamic desktop = shell.Windows().FindWindowSW(ref location, ref root, 8, out int window, 1);
            if (window == 0 || desktop is null) throw new CaptureException("E_BROWSER_DESKTOP_SHELL_UNAVAILABLE");
            desktop.Document.Application.ShellExecute(exe, arguments, Path.GetDirectoryName(exe), "open", 1);
        }
        catch (COMException ex)
        {
            throw new CaptureException("E_BROWSER_DESKTOP_SHELL_UNAVAILABLE: " + ex.Message);
        }
    }

    private static IntPtr OpenManagement(Browser browser, string token, CancellationToken cancellation)
    {
        ValidateToken(token);
        var alreadyRunning = BrowserProcesses(browser).Count > 0;
        var arguments = (alreadyRunning ? "" : "--force-renderer-accessibility ") + browser.Page(token);
        cancellation.ThrowIfCancellationRequested();
        StartInDesktop(FindExecutable(browser), arguments);
        var window = UntilHandle(() => ManagementWindow(browser, token, wake: true), TimeSpan.FromSeconds(7),
            cancellation, "E_BROWSER_EXTENSIONS_PAGE_NOT_FOUND");
        WindowsTools.ForceForeground(window);
        WakeAccessibility(window);
        return window;
    }

    private static bool CloseManagement(Browser browser, string token, CancellationToken cancellation)
    {
        ValidateToken(token);
        var window = ManagementWindow(browser, token, wake: false);
        if (window == IntPtr.Zero) return false;
        if (!WindowsTools.ForceForeground(window) || !MatchesAddress(window, token)) return false;
        WindowsInput.Keys(["Ctrl", "W"], cancellation);
        return true;
    }

    private static IntPtr ManagementWindow(Browser browser, string token, bool wake)
    {
        var processes = BrowserProcesses(browser);
        if (processes.Count == 0) return IntPtr.Zero;
        var foreground = GetForegroundWindow();
        var windows = TopWindows().Where(handle => ClassName(handle) == "Chrome_WidgetWin_1" &&
            processes.Contains(ProcessId(handle))).OrderByDescending(handle => handle == foreground);
        foreach (var window in windows)
        {
            if (wake) WakeAccessibility(window);
            if (MatchesAddress(window, token)) return window;
        }
        return IntPtr.Zero;
    }

    private static bool MatchesAddress(IntPtr window, string token)
    {
        try
        {
            var root = AutomationElement.FromHandle(window);
            var candidates = root.FindAll(TreeScope.Descendants, new OrCondition(
                new PropertyCondition(AutomationElement.ControlTypeProperty, ControlType.Edit),
                new PropertyCondition(AutomationElement.ControlTypeProperty, ControlType.Document)));
            foreach (AutomationElement candidate in candidates)
            {
                if (!candidate.TryGetCurrentPattern(ValuePattern.Pattern, out var pattern)) continue;
                var value = ((ValuePattern)pattern).Current.Value?.Trim() ?? "";
                if (value.Length == 0) continue;
                if (!value.Contains("://", StringComparison.Ordinal) &&
                    value.StartsWith("extensions", StringComparison.OrdinalIgnoreCase))
                {
                    value = "chrome://" + value;
                }
                if (Uri.TryCreate(value, UriKind.Absolute, out var uri) && uri.Scheme is "chrome" or "edge" &&
                    uri.Host.Equals("extensions", StringComparison.OrdinalIgnoreCase) &&
                    uri.Query.TrimStart('?') == "ghosthand=" + token)
                    return true;
            }
        }
        catch (Exception ex) when (ex is ElementNotAvailableException or COMException)
        {
        }
        return false;
    }

    private static void WakeAccessibility(IntPtr window)
    {
        var iid = new Guid("618736e0-3c3d-11cf-810c-00aa00389b71");
        SendMessageTimeout(window, WM_GETOBJECT, IntPtr.Zero, new IntPtr(-4), SMTO_ABORTIFHUNG, 75, out _);
        _ = AccessibleObjectFromWindow(window, unchecked((uint)-4), ref iid, out _);
        EnumChildWindows(window, (child, unused) =>
        {
            if (ClassName(child) == "Chrome_RenderWidgetHostHWND")
            {
                SendMessageTimeout(child, WM_GETOBJECT, IntPtr.Zero, new IntPtr(-4), SMTO_ABORTIFHUNG, 75, out _);
                _ = AccessibleObjectFromWindow(child, unchecked((uint)-4), ref iid, out _);
            }
            return true;
        }, IntPtr.Zero);
    }

    private static AutomationElement? FindLoadButton(IntPtr window) =>
        Find(window, "loadUnpacked", ["Load unpacked", "加载已解压的扩展程序", "載入未封裝項目", "載入未封裝擴充功能"], preferButton: true);

    private static AutomationElement? FindDevModeToggle(IntPtr window)
    {
        try
        {
            var root = AutomationElement.FromHandle(window);
            foreach (var id in new[] { "devMode", "developer-mode" })
            {
                var byId = root.FindFirst(TreeScope.Descendants, new PropertyCondition(AutomationElement.AutomationIdProperty, id));
                if (byId is not null && byId.Current.IsEnabled && !byId.Current.IsOffscreen) return byId;
            }
            var condition = new OrCondition(
                new PropertyCondition(AutomationElement.ControlTypeProperty, ControlType.CheckBox),
                new PropertyCondition(AutomationElement.ControlTypeProperty, ControlType.Button));
            string[] names = ["Developer mode", "开发者模式", "開發人員模式"];
            return root.FindAll(TreeScope.Descendants, condition).Cast<AutomationElement>().FirstOrDefault(el =>
                el.Current.IsEnabled && !el.Current.IsOffscreen &&
                names.Any(name => el.Current.Name.Contains(name, StringComparison.OrdinalIgnoreCase)));
        }
        catch (Exception ex) when (ex is ElementNotAvailableException or COMException)
        {
            return null;
        }
    }

    private static AutomationElement? Find(IntPtr window, string id, string[] names, bool preferButton = false)
    {
        try
        {
            var root = AutomationElement.FromHandle(window);
            var result = root.FindFirst(TreeScope.Descendants, new PropertyCondition(AutomationElement.AutomationIdProperty, id));
            if (result is not null && result.Current.IsEnabled && !result.Current.IsOffscreen) return result;
            var condition = preferButton
                ? new PropertyCondition(AutomationElement.ControlTypeProperty, ControlType.Button)
                : Automation.ControlViewCondition;
            return root.FindAll(TreeScope.Descendants, condition).Cast<AutomationElement>().FirstOrDefault(el =>
                el.Current.IsEnabled && !el.Current.IsOffscreen &&
                names.Any(name => el.Current.Name.Contains(name, StringComparison.OrdinalIgnoreCase)));
        }
        catch (Exception ex) when (ex is ElementNotAvailableException or COMException)
        {
            return null;
        }
    }

    private static AutomationElement? FindCard(IntPtr window)
    {
        try
        {
            var root = AutomationElement.FromHandle(window);
            return root.FindFirst(TreeScope.Descendants, new PropertyCondition(AutomationElement.AutomationIdProperty, ExtensionId))
                ?? root.FindFirst(TreeScope.Descendants, new AndCondition(
                    new PropertyCondition(AutomationElement.NameProperty, ExtensionName),
                    new NotCondition(new PropertyCondition(AutomationElement.ControlTypeProperty, ControlType.TabItem))));
        }
        catch (Exception ex) when (ex is ElementNotAvailableException or COMException)
        {
            return null;
        }
    }

    private static AutomationElement? FindCardControl(AutomationElement card, string id, string[] labels)
    {
        for (var depth = 0; card is not null && depth < 6; depth++, card = TreeWalker.ControlViewWalker.GetParent(card))
        {
            if (card.Current.ControlType == ControlType.Document) break;
            var named = card.FindAll(TreeScope.Descendants, new PropertyCondition(AutomationElement.AutomationIdProperty, id))
                .Cast<AutomationElement>().Where(el => el.Current.IsEnabled && !el.Current.IsOffscreen).ToArray();
            if (named.Length == 1) return named[0];
            if (named.Length > 1) return null;
            var buttons = card.FindAll(TreeScope.Descendants, new OrCondition(
                new PropertyCondition(AutomationElement.ControlTypeProperty, ControlType.Button),
                new PropertyCondition(AutomationElement.ControlTypeProperty, ControlType.CheckBox))).Cast<AutomationElement>();
            var matches = buttons.Where(el => el.Current.IsEnabled && !el.Current.IsOffscreen &&
                labels.Any(label => el.Current.Name.Equals(label, StringComparison.OrdinalIgnoreCase) ||
                    el.Current.Name.StartsWith($"{label} {ExtensionName}", StringComparison.OrdinalIgnoreCase))).ToArray();
            if (matches.Length == 1) return matches[0];
            if (matches.Length > 1) return null;
        }
        return null;
    }

    private static AutomationElement? FindRemoveConfirmation(IntPtr window, AutomationElement original)
    {
        foreach (var handle in Dialogs(window).Prepend(window))
        {
            try
            {
                var root = AutomationElement.FromHandle(handle);
                var buttons = root.FindAll(TreeScope.Descendants,
                    new PropertyCondition(AutomationElement.ControlTypeProperty, ControlType.Button));
                foreach (AutomationElement button in buttons)
                {
                    if (button.Equals(original) || !button.Current.IsEnabled || button.Current.IsOffscreen ||
                        button.Current.AutomationId == "removeButton") continue;
                    if (button.Current.Name is "Remove" or "移除" or "删除" or "移除擴充功能")
                    {
                        if (handle != window || HasDialogAncestor(button)) return button;
                    }
                }
            }
            catch (Exception ex) when (ex is ElementNotAvailableException or COMException)
            {
            }
        }
        return null;
    }

    private static bool HasDialogAncestor(AutomationElement button)
    {
        var walker = TreeWalker.RawViewWalker;
        for (var item = walker.GetParent(button); item is not null; item = walker.GetParent(item))
        {
            var localized = item.Current.LocalizedControlType;
            if (localized is "dialog" or "对话框" or "對話框" ||
                item.TryGetCurrentPattern(WindowPattern.Pattern, out var pattern) && ((WindowPattern)pattern).Current.IsModal)
                return true;
            if (item.Current.ControlType == ControlType.Document) break;
        }
        return false;
    }

    private static void Invoke(AutomationElement element, CancellationToken cancellation)
    {
        cancellation.ThrowIfCancellationRequested();
        if (element.TryGetCurrentPattern(InvokePattern.Pattern, out var pattern))
        {
            ((InvokePattern)pattern).Invoke();
            return;
        }
        if (element.TryGetCurrentPattern(TogglePattern.Pattern, out var toggle))
        {
            ((TogglePattern)toggle).Toggle();
            return;
        }
        try
        {
            element.SetFocus();
            WindowsInput.Keys(["Space"], cancellation);
            return;
        }
        catch (Exception ex) when (ex is InvalidOperationException or ElementNotAvailableException or COMException)
        {
        }
        var bounds = element.Current.BoundingRectangle;
        if (!bounds.IsEmpty && bounds.Width > 0 && bounds.Height > 0)
        {
            WindowsInput.Click((int)(bounds.Left + bounds.Width / 2), (int)(bounds.Top + bounds.Height / 2), "left", false, false, modifiers: null, token: cancellation);
        }
    }

    private static void SetFolder(IntPtr dialog, string directory, CancellationToken cancellation)
    {
        cancellation.ThrowIfCancellationRequested();
        var edit = IntPtr.Zero;
        var field = GetDlgItem(dialog, 0x47c);
        if (field != IntPtr.Zero)
        {
            EnumChildWindows(field, (child, _) =>
            {
                if (IsWindowVisible(child) && ClassName(child) == "Edit") edit = child;
                return edit == IntPtr.Zero;
            }, IntPtr.Zero);
        }
        if (edit == IntPtr.Zero)
        {
            var lowest = int.MinValue;
            EnumChildWindows(dialog, (child, _) =>
            {
                if (IsWindowVisible(child) && IsWindowEnabled(child) && ClassName(child) == "Edit" &&
                    GetWindowRect(child, out var rect) && rect.Top > lowest)
                {
                    edit = child;
                    lowest = rect.Top;
                }
                return true;
            }, IntPtr.Zero);
        }
        if (edit == IntPtr.Zero) throw new CaptureException("E_BROWSER_FOLDER_FIELD_NOT_FOUND");
        var wroteViaUia = false;
        try
        {
            var fieldElement = AutomationElement.FromHandle(edit);
            if (fieldElement.TryGetCurrentPattern(ValuePattern.Pattern, out var value))
            {
                ((ValuePattern)value).SetValue(directory);
                wroteViaUia = true;
            }
        }
        catch (Exception ex) when (ex is ElementNotAvailableException or InvalidOperationException or COMException)
        {
        }
        if (!wroteViaUia)
        {
            if (SendMessageTimeoutText(edit, WM_SETTEXT, IntPtr.Zero, directory, SMTO_ABORTIFHUNG, 800, out var written) == IntPtr.Zero ||
                written == IntPtr.Zero)
            {
                throw new CaptureException("E_BROWSER_FOLDER_FIELD_WRITE");
            }
            var parent = GetParent(edit);
            if (parent != IntPtr.Zero)
            {
                var ctrlId = GetDlgCtrlID(edit);
                SendMessageTimeout(parent, 0x0111 /* WM_COMMAND */, new IntPtr((0x0300 /* EN_CHANGE */ << 16) | (ctrlId & 0xffff)), edit, SMTO_ABORTIFHUNG, 200, out _);
            }
        }
        var ok = GetDlgItem(dialog, IDOK);
        if (ok == IntPtr.Zero || !IsWindowEnabled(ok)) throw new CaptureException("E_BROWSER_FOLDER_CONFIRM_NOT_FOUND");
        cancellation.ThrowIfCancellationRequested();
        if (!PostMessage(ok, BM_CLICK, IntPtr.Zero, IntPtr.Zero)) throw new CaptureException("E_BROWSER_FOLDER_CONFIRM_FAILED");
        var clickedAt = Stopwatch.StartNew();
        var retriedClick = false;
        Until(() =>
        {
            if (!IsWindowVisible(dialog)) return new object();
            if (!retriedClick && clickedAt.ElapsedMilliseconds >= 450 && IsWindowEnabled(ok))
            {
                retriedClick = true;
                PostMessage(ok, BM_CLICK, IntPtr.Zero, IntPtr.Zero);
            }
            return null;
        }, TimeSpan.FromSeconds(4), cancellation, "E_BROWSER_FOLDER_CONFIRM_TIMEOUT");
    }

    private static IEnumerable<IntPtr> Dialogs(IntPtr owner)
    {
        var pid = ProcessId(owner);
        return TopWindows().Where(handle => ClassName(handle) == "#32770" &&
            (GetWindow(handle, GW_OWNER) == owner || ProcessId(handle) == pid));
    }

    private static List<IntPtr> TopWindows()
    {
        var result = new List<IntPtr>();
        EnumWindows((window, _) =>
        {
            if (IsWindowVisible(window)) result.Add(window);
            return true;
        }, IntPtr.Zero);
        return result;
    }

    private static uint ProcessId(IntPtr window)
    {
        GetWindowThreadProcessId(window, out var pid);
        return pid;
    }

    private static string ClassName(IntPtr window)
    {
        var value = new System.Text.StringBuilder(64);
        GetClassName(window, value, value.Capacity);
        return value.ToString();
    }

    private static T Until<T>(Func<T?> find, TimeSpan timeout, CancellationToken cancellation, string error) where T : class
    {
        var elapsed = Stopwatch.StartNew();
        do
        {
            cancellation.ThrowIfCancellationRequested();
            if (find() is { } result) return result;
        } while (!cancellation.WaitHandle.WaitOne(60) && elapsed.Elapsed < timeout);
        cancellation.ThrowIfCancellationRequested();
        throw new CaptureException(error);
    }

    private static IntPtr UntilHandle(Func<IntPtr> find, TimeSpan timeout, CancellationToken cancellation, string error) =>
        Until(() => find() is var handle && handle != IntPtr.Zero ? Tuple.Create(handle) : null, timeout, cancellation, error).Item1;

    private static void Observe(Task task)
    {
        _ = task.ContinueWith(item => { _ = item.Exception; }, TaskContinuationOptions.OnlyOnFaulted | TaskContinuationOptions.ExecuteSynchronously);
    }

    [StructLayout(LayoutKind.Sequential)]
    private struct Rect { public int Left, Top, Right, Bottom; }
    private delegate bool WindowCallback(IntPtr window, IntPtr parameter);
    [DllImport("user32.dll")] private static extern bool EnumWindows(WindowCallback callback, IntPtr parameter);
    [DllImport("user32.dll")] private static extern bool EnumChildWindows(IntPtr window, WindowCallback callback, IntPtr parameter);
    [DllImport("user32.dll")] private static extern IntPtr GetWindow(IntPtr window, uint command);
    [DllImport("user32.dll")] private static extern bool IsWindowVisible(IntPtr window);
    [DllImport("user32.dll")] private static extern bool IsWindowEnabled(IntPtr window);
    [DllImport("user32.dll")] private static extern bool GetWindowRect(IntPtr window, out Rect rect);
    [DllImport("user32.dll")] private static extern IntPtr GetForegroundWindow();
    [DllImport("user32.dll")] private static extern uint GetWindowThreadProcessId(IntPtr window, out uint processId);
    [DllImport("user32.dll", CharSet = CharSet.Unicode)] private static extern int GetClassName(IntPtr window, System.Text.StringBuilder name, int size);
    [DllImport("user32.dll")] private static extern IntPtr GetDlgItem(IntPtr dialog, int id);
    [DllImport("user32.dll")] private static extern IntPtr GetParent(IntPtr window);
    [DllImport("user32.dll")] private static extern int GetDlgCtrlID(IntPtr window);
    [DllImport("oleacc.dll")] private static extern int AccessibleObjectFromWindow(
        IntPtr hwnd, uint dwId, ref Guid riid, [MarshalAs(UnmanagedType.IUnknown)] out object? ppvObject);
    [DllImport("user32.dll", EntryPoint = "SendMessageTimeoutW")] private static extern IntPtr SendMessageTimeout(
        IntPtr window, uint message, IntPtr wParam, IntPtr lParam, uint flags, uint timeout, out IntPtr result);
    [DllImport("user32.dll", EntryPoint = "SendMessageTimeoutW", CharSet = CharSet.Unicode)] private static extern IntPtr SendMessageTimeoutText(
        IntPtr window, uint message, IntPtr wParam, string text, uint flags, uint timeout, out IntPtr result);
    [DllImport("user32.dll", EntryPoint = "PostMessageW")] private static extern bool PostMessage(
        IntPtr window, uint message, IntPtr wParam, IntPtr lParam);
}
