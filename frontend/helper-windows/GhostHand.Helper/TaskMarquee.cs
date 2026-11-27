using System.Runtime.InteropServices;
using System.Windows;
using System.Windows.Controls;
using System.Windows.Controls.Primitives;
using System.Windows.Input;
using System.Windows.Interop;
using System.Windows.Media;
using System.Windows.Media.Effects;
using System.Windows.Shapes;
using System.Windows.Threading;

namespace GhostHand.Helper;

internal sealed class TaskMarquee : IDisposable
{
    private readonly Window edgeFrame = CreateEdgeWindow();
    private readonly Window banner = CreateFloatingWindow(transparentToMouse: true);
    private readonly Window actions = CreateFloatingWindow(transparentToMouse: false);

    private readonly SolidColorBrush edgeOuterBrush = new(Color.FromArgb(235, 6, 182, 212));
    private readonly SolidColorBrush edgeMidBrush = new(Color.FromArgb(115, 99, 102, 241));
    private readonly SolidColorBrush edgeInnerBrush = new(Color.FromArgb(45, 6, 182, 212));
    private readonly SolidColorBrush bannerBorderBrush = new(Color.FromArgb(220, 99, 102, 241));
    private readonly SolidColorBrush statusDotBrush = new(Color.FromRgb(34, 211, 238));
    private readonly SolidColorBrush statusBadgeBgBrush = new(Color.FromArgb(55, 99, 102, 241));
    private readonly SolidColorBrush statusBadgeFgBrush = new(Color.FromRgb(165, 180, 252));

    private readonly Ellipse statusDot;
    private readonly TextBlock statusBadgeText;
    private readonly TextBlock titleLabel;
    private readonly TextBlock detailLabel;
    private readonly Button toggle;
    private readonly Button stop;

    private readonly DispatcherTimer flashTimer;
    private readonly Func<string, string, Task> send;

    private string? task;
    private string? hiddenForCapture;
    private string currentState = "running";
    private bool paused;
    private double flashPhase;

    public TaskMarquee(Func<string, string, Task> send)
    {
        this.send = send;

        edgeFrame.Content = BuildEdgeOverlay();

        statusDot = new Ellipse
        {
            Width = 9,
            Height = 9,
            Fill = statusDotBrush,
            VerticalAlignment = VerticalAlignment.Center,
            Margin = new Thickness(0, 0, 9, 0)
        };

        statusBadgeText = new TextBlock
        {
            Text = "正在执行",
            FontSize = 11.5,
            FontWeight = FontWeights.SemiBold,
            Foreground = statusBadgeFgBrush,
            VerticalAlignment = VerticalAlignment.Center
        };

        var badge = new Border
        {
            Background = statusBadgeBgBrush,
            CornerRadius = new CornerRadius(6),
            Padding = new Thickness(8, 3, 8, 3),
            Margin = new Thickness(0, 0, 10, 0),
            VerticalAlignment = VerticalAlignment.Center,
            Child = statusBadgeText
        };

        titleLabel = new TextBlock
        {
            Foreground = Brushes.White,
            FontSize = 13.5,
            FontWeight = FontWeights.SemiBold,
            TextWrapping = TextWrapping.Wrap,
            LineHeight = 19
        };

        detailLabel = new TextBlock
        {
            Foreground = new SolidColorBrush(Color.FromRgb(203, 213, 225)),
            FontSize = 12,
            TextWrapping = TextWrapping.Wrap,
            LineHeight = 17,
            Margin = new Thickness(0, 2, 0, 0),
            Visibility = Visibility.Collapsed
        };

        var textStack = new StackPanel
        {
            VerticalAlignment = VerticalAlignment.Center
        };
        textStack.Children.Add(titleLabel);
        textStack.Children.Add(detailLabel);

        var row = new Grid();
        row.ColumnDefinitions.Add(new ColumnDefinition { Width = GridLength.Auto });
        row.ColumnDefinitions.Add(new ColumnDefinition { Width = GridLength.Auto });
        row.ColumnDefinitions.Add(new ColumnDefinition { Width = new GridLength(1, GridUnitType.Star) });
        Grid.SetColumn(statusDot, 0);
        Grid.SetColumn(badge, 1);
        Grid.SetColumn(textStack, 2);
        row.Children.Add(statusDot);
        row.Children.Add(badge);
        row.Children.Add(textStack);

        banner.Content = CreateCardContainer(row, new Thickness(15, 10, 16, 10), new Thickness(8, 4, 6, 12));

        toggle = new Button
        {
            Content = "暂停",
            Height = 30,
            MinWidth = 60,
            Margin = new Thickness(0, 0, 6, 0),
            Focusable = false,
            Style = CreateButtonStyle(
                normalBg: Color.FromRgb(39, 43, 59),
                hoverBg: Color.FromRgb(55, 62, 86),
                pressedBg: Color.FromRgb(30, 33, 46),
                normalBorder: Color.FromRgb(71, 85, 105),
                hoverBorder: Color.FromRgb(129, 140, 248),
                normalFg: Color.FromRgb(241, 245, 249),
                hoverFg: Colors.White)
        };

        stop = new Button
        {
            Content = "终止",
            Height = 30,
            MinWidth = 60,
            Focusable = false,
            Style = CreateButtonStyle(
                normalBg: Color.FromRgb(68, 24, 36),
                hoverBg: Color.FromRgb(225, 29, 72),
                pressedBg: Color.FromRgb(159, 18, 57),
                normalBorder: Color.FromRgb(190, 18, 60),
                hoverBorder: Color.FromRgb(251, 113, 133),
                normalFg: Color.FromRgb(254, 205, 211),
                hoverFg: Colors.White)
        };

        var buttons = new StackPanel
        {
            Orientation = Orientation.Horizontal,
            VerticalAlignment = VerticalAlignment.Center
        };
        buttons.Children.Add(toggle);
        buttons.Children.Add(stop);
        actions.Content = CreateCardContainer(buttons, new Thickness(7), new Thickness(2, 4, 8, 12));

        toggle.Click += async (_, _) => await SendAsync(paused ? "resume" : "pause");
        stop.Click += async (_, _) => await SendAsync("cancel");

        banner.SizeChanged += (_, _) =>
        {
            if (banner.IsVisible)
            {
                PositionTopBar();
            }
        };
        actions.SizeChanged += (_, _) =>
        {
            if (actions.IsVisible)
            {
                PositionTopBar();
            }
        };

        flashTimer = new DispatcherTimer(DispatcherPriority.Render)
        {
            Interval = TimeSpan.FromMilliseconds(40)
        };
        flashTimer.Tick += (_, _) => StepFlash();
    }

    public void Update(string id, bool visible, string state, string title, int step, int total, string stepName)
    {
        if (!visible)
        {
            if (task == id)
            {
                task = null;
                Hide();
            }
            return;
        }
        task = id;
        currentState = state;
        paused = state == "paused";
        toggle.IsEnabled = state is "running" or "paused";
        stop.IsEnabled = state != "cancelling";
        toggle.Content = paused ? "继续" : "暂停";

        statusBadgeText.Text = state switch
        {
            "paused" => "已暂停",
            "waiting_user" => "等待确认",
            "cancelling" => "终止中",
            _ => "正在执行"
        };
        ApplyBadgePalette();

        var cleanTitle = string.IsNullOrWhiteSpace(title) ? "设备任务" : title.Trim();
        titleLabel.Text = $"GhostHand · {cleanTitle}";

        var cleanStep = stepName.Trim();
        var detail = step > 0 && total > 0
            ? (cleanStep.Length > 0 ? $"步骤 {step}/{total} · {cleanStep}" : $"步骤 {step}/{total}")
            : cleanStep;
        if (detail.Length > 0)
        {
            detailLabel.Text = detail;
            detailLabel.Visibility = Visibility.Visible;
        }
        else
        {
            detailLabel.Text = "";
            detailLabel.Visibility = Visibility.Collapsed;
        }

        if (hiddenForCapture != id)
        {
            Show();
        }
    }

    public async Task PrepareCaptureAsync(string id, CancellationToken token)
    {
        if (task != id || (!banner.IsVisible && !actions.IsVisible && !edgeFrame.IsVisible))
        {
            return;
        }
        // Some display drivers reject affinity on layered WPF windows.
        if (Exclude(edgeFrame) && Exclude(banner) && Exclude(actions))
        {
            return;
        }
        hiddenForCapture = id;
        Hide();
        await banner.Dispatcher.InvokeAsync(() => { }, DispatcherPriority.ContextIdle, token);
        await Task.Delay(40, token);
    }

    public void FinishCapture(string id)
    {
        if (hiddenForCapture == id)
        {
            hiddenForCapture = null;
            if (task == id)
            {
                Show();
            }
        }
    }

    private async Task SendAsync(string action)
    {
        if (task is string id)
        {
            await send(id, action);
        }
    }

    private void Show()
    {
        var area = SystemParameters.WorkArea;
        banner.MaxWidth = Math.Max(260, Math.Min(area.Width - 180, area.Width * 0.74));

        edgeFrame.Left = 0;
        edgeFrame.Top = 0;
        edgeFrame.Width = SystemParameters.PrimaryScreenWidth;
        edgeFrame.Height = SystemParameters.PrimaryScreenHeight;

        StepFlash();
        if (!flashTimer.IsEnabled)
        {
            flashTimer.Start();
        }

        edgeFrame.Show();
        banner.Show();
        actions.Show();
        PositionTopBar();
    }

    private void PositionTopBar()
    {
        var area = SystemParameters.WorkArea;
        var bannerW = banner.ActualWidth > 0 ? banner.ActualWidth : banner.DesiredSize.Width;
        var actionsW = actions.ActualWidth > 0 ? actions.ActualWidth : actions.DesiredSize.Width;
        var bannerH = banner.ActualHeight > 0 ? banner.ActualHeight : banner.DesiredSize.Height;
        var actionsH = actions.ActualHeight > 0 ? actions.ActualHeight : actions.DesiredSize.Height;
        const double visualGap = 0;
        var totalW = bannerW + visualGap + actionsW;
        var left = area.Left + Math.Max(0, (area.Width - totalW) / 2);
        var top = area.Top + 8;
        banner.Left = left;
        banner.Top = top;
        actions.Left = left + bannerW + visualGap;
        actions.Top = top + Math.Max(0, (bannerH - actionsH) / 2);
    }

    private void Hide()
    {
        flashTimer.Stop();
        edgeFrame.Hide();
        banner.Hide();
        actions.Hide();
    }

    public void Dispose()
    {
        flashTimer.Stop();
        edgeFrame.Close();
        banner.Close();
        actions.Close();
    }

    private void ApplyBadgePalette()
    {
        if (paused)
        {
            statusBadgeBgBrush.Color = Color.FromArgb(60, 100, 116, 139);
            statusBadgeFgBrush.Color = Color.FromRgb(203, 213, 225);
        }
        else if (currentState == "waiting_user")
        {
            statusBadgeBgBrush.Color = Color.FromArgb(65, 245, 158, 11);
            statusBadgeFgBrush.Color = Color.FromRgb(253, 224, 71);
        }
        else if (currentState == "cancelling")
        {
            statusBadgeBgBrush.Color = Color.FromArgb(65, 225, 29, 72);
            statusBadgeFgBrush.Color = Color.FromRgb(254, 205, 211);
        }
        else
        {
            statusBadgeBgBrush.Color = Color.FromArgb(60, 79, 70, 229);
            statusBadgeFgBrush.Color = Color.FromRgb(165, 243, 252);
        }
    }

    private void StepFlash()
    {
        flashPhase += paused ? 0.06 : 0.14;
        var t = (Math.Sin(flashPhase) + 1.0) * 0.5;

        Color primaryA;
        Color primaryB;
        if (paused)
        {
            primaryA = Color.FromRgb(100, 116, 139); // Slate
            primaryB = Color.FromRgb(148, 163, 184); // Light slate
        }
        else if (currentState == "waiting_user")
        {
            primaryA = Color.FromRgb(245, 158, 11);  // Amber
            primaryB = Color.FromRgb(239, 68, 68);   // Coral red
        }
        else if (currentState == "cancelling")
        {
            primaryA = Color.FromRgb(225, 29, 72);
            primaryB = Color.FromRgb(251, 113, 133);
        }
        else
        {
            primaryA = Color.FromRgb(6, 182, 212);   // Cyan
            primaryB = Color.FromRgb(124, 58, 237);  // Electric violet
        }

        var c1 = LerpColor(primaryA, primaryB, t);
        var c2 = LerpColor(primaryB, primaryA, t);
        var outerAlpha = paused ? (byte)155 : (byte)(185 + 70 * t);
        var midAlpha = paused ? (byte)55 : (byte)(75 + 55 * t);
        var innerAlpha = paused ? (byte)20 : (byte)(28 + 30 * t);

        edgeOuterBrush.Color = Color.FromArgb(outerAlpha, c1.R, c1.G, c1.B);
        edgeMidBrush.Color = Color.FromArgb(midAlpha, c2.R, c2.G, c2.B);
        edgeInnerBrush.Color = Color.FromArgb(innerAlpha, c1.R, c1.G, c1.B);
        bannerBorderBrush.Color = Color.FromArgb(225, c1.R, c1.G, c1.B);
        statusDotBrush.Color = c1;
    }

    private static Color LerpColor(Color a, Color b, double t) => Color.FromRgb(
        (byte)Math.Round(a.R + (b.R - a.R) * t),
        (byte)Math.Round(a.G + (b.G - a.G) * t),
        (byte)Math.Round(a.B + (b.B - a.B) * t));

    private UIElement BuildEdgeOverlay()
    {
        var inner = new Border
        {
            BorderBrush = edgeInnerBrush,
            BorderThickness = new Thickness(6),
            IsHitTestVisible = false
        };
        var mid = new Border
        {
            BorderBrush = edgeMidBrush,
            BorderThickness = new Thickness(4),
            Child = inner,
            IsHitTestVisible = false
        };
        return new Border
        {
            BorderBrush = edgeOuterBrush,
            BorderThickness = new Thickness(3),
            Child = mid,
            IsHitTestVisible = false
        };
    }

    private Border CreateCardContainer(UIElement content, Thickness padding, Thickness margin) => new()
    {
        Margin = margin,
        Padding = padding,
        CornerRadius = new CornerRadius(14),
        Background = new SolidColorBrush(Color.FromArgb(242, 18, 20, 28)),
        BorderBrush = bannerBorderBrush,
        BorderThickness = new Thickness(1.5),
        Effect = new DropShadowEffect
        {
            Color = Colors.Black,
            BlurRadius = 16,
            ShadowDepth = 3,
            Opacity = 0.45
        },
        Child = content
    };

    private static Style CreateButtonStyle(
        Color normalBg, Color hoverBg, Color pressedBg,
        Color normalBorder, Color hoverBorder,
        Color normalFg, Color hoverFg)
    {
        var style = new Style(typeof(Button));
        style.Setters.Add(new Setter(Control.ForegroundProperty, new SolidColorBrush(normalFg)));
        style.Setters.Add(new Setter(Control.BackgroundProperty, new SolidColorBrush(normalBg)));
        style.Setters.Add(new Setter(Control.BorderBrushProperty, new SolidColorBrush(normalBorder)));
        style.Setters.Add(new Setter(Control.BorderThicknessProperty, new Thickness(1)));
        style.Setters.Add(new Setter(FrameworkElement.CursorProperty, Cursors.Hand));
        style.Setters.Add(new Setter(Control.FontSizeProperty, 12.5));
        style.Setters.Add(new Setter(Control.FontWeightProperty, FontWeights.SemiBold));
        style.Setters.Add(new Setter(Control.PaddingProperty, new Thickness(12, 0, 12, 0)));

        var template = new ControlTemplate(typeof(Button));
        var borderFactory = new FrameworkElementFactory(typeof(Border), "border");
        borderFactory.SetValue(Border.CornerRadiusProperty, new CornerRadius(9));
        borderFactory.SetValue(Border.BackgroundProperty, new TemplateBindingExtension(Control.BackgroundProperty));
        borderFactory.SetValue(Border.BorderBrushProperty, new TemplateBindingExtension(Control.BorderBrushProperty));
        borderFactory.SetValue(Border.BorderThicknessProperty, new TemplateBindingExtension(Control.BorderThicknessProperty));
        borderFactory.SetValue(Border.PaddingProperty, new TemplateBindingExtension(Control.PaddingProperty));
        borderFactory.SetValue(Border.SnapsToDevicePixelsProperty, true);

        var presenterFactory = new FrameworkElementFactory(typeof(ContentPresenter));
        presenterFactory.SetValue(FrameworkElement.HorizontalAlignmentProperty, HorizontalAlignment.Center);
        presenterFactory.SetValue(FrameworkElement.VerticalAlignmentProperty, VerticalAlignment.Center);
        borderFactory.AppendChild(presenterFactory);
        template.VisualTree = borderFactory;

        var hoverTrigger = new Trigger { Property = UIElement.IsMouseOverProperty, Value = true };
        hoverTrigger.Setters.Add(new Setter(Control.BackgroundProperty, new SolidColorBrush(hoverBg)));
        hoverTrigger.Setters.Add(new Setter(Control.BorderBrushProperty, new SolidColorBrush(hoverBorder)));
        hoverTrigger.Setters.Add(new Setter(Control.ForegroundProperty, new SolidColorBrush(hoverFg)));
        template.Triggers.Add(hoverTrigger);

        var pressedTrigger = new Trigger { Property = ButtonBase.IsPressedProperty, Value = true };
        pressedTrigger.Setters.Add(new Setter(Control.BackgroundProperty, new SolidColorBrush(pressedBg)));
        pressedTrigger.Setters.Add(new Setter(Control.BorderBrushProperty, new SolidColorBrush(hoverBorder)));
        pressedTrigger.Setters.Add(new Setter(Control.ForegroundProperty, new SolidColorBrush(hoverFg)));
        template.Triggers.Add(pressedTrigger);

        var disabledTrigger = new Trigger { Property = UIElement.IsEnabledProperty, Value = false };
        disabledTrigger.Setters.Add(new Setter(UIElement.OpacityProperty, 0.42));
        template.Triggers.Add(disabledTrigger);

        style.Setters.Add(new Setter(Control.TemplateProperty, template));
        return style;
    }

    private static Window CreateEdgeWindow()
    {
        var window = new Window
        {
            Title = "GhostHand Edge",
            Left = 0,
            Top = 0,
            Width = SystemParameters.PrimaryScreenWidth,
            Height = SystemParameters.PrimaryScreenHeight,
            WindowStyle = WindowStyle.None,
            ResizeMode = ResizeMode.NoResize,
            AllowsTransparency = true,
            Background = Brushes.Transparent,
            ShowActivated = false,
            ShowInTaskbar = false,
            Topmost = true,
            IsHitTestVisible = false
        };
        ConfigureOverlayHandle(window, transparentToMouse: true);
        return window;
    }

    private static Window CreateFloatingWindow(bool transparentToMouse)
    {
        var window = new Window
        {
            Title = "GhostHand",
            SizeToContent = SizeToContent.WidthAndHeight,
            WindowStyle = WindowStyle.None,
            ResizeMode = ResizeMode.NoResize,
            AllowsTransparency = true,
            Background = Brushes.Transparent,
            ShowActivated = false,
            ShowInTaskbar = false,
            Topmost = true,
            FontFamily = new FontFamily("Segoe UI, Microsoft YaHei")
        };
        ConfigureOverlayHandle(window, transparentToMouse);
        return window;
    }

    private static void ConfigureOverlayHandle(Window window, bool transparentToMouse)
    {
        window.SourceInitialized += (_, _) =>
        {
            var handle = new WindowInteropHelper(window).Handle;
            var style = GetWindowLongPtr(handle, -20).ToInt64();
            const long noActivate = 0x08000000;
            const long toolWindow = 0x80;
            const long transparent = 0x20;
            SetWindowLongPtr(handle, -20, new IntPtr(style | noActivate | toolWindow | (transparentToMouse ? transparent : 0)));
            Exclude(window);
        };
    }

    private static bool Exclude(Window window) =>
        SetWindowDisplayAffinity(new WindowInteropHelper(window).Handle, 0x11);

    [DllImport("user32.dll", EntryPoint = "GetWindowLongPtrW")]
    private static extern IntPtr GetWindowLongPtr(IntPtr window, int index);
    [DllImport("user32.dll", EntryPoint = "SetWindowLongPtrW")]
    private static extern IntPtr SetWindowLongPtr(IntPtr window, int index, IntPtr value);
    [DllImport("user32.dll")]
    private static extern bool SetWindowDisplayAffinity(IntPtr window, uint affinity);
}
