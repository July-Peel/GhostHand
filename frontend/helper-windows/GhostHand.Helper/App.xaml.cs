using System.IO;
using System.Windows;

namespace GhostHand.Helper;

public partial class App : Application
{
    protected override async void OnStartup(StartupEventArgs e)
    {
        base.OnStartup(e);

        if (e.Args.Length == 0)
        {
            ShutdownMode = ShutdownMode.OnMainWindowClose;
            MainWindow = new MainWindow();
            MainWindow.Show();
            return;
        }

        ShutdownMode = ShutdownMode.OnExplicitShutdown;
        if (e.Args.Length != 5 || e.Args[0] != "--service" ||
            e.Args[1] != "--pipe" || e.Args[2] != "ghosthand-core" ||
            e.Args[3] != "--nonce" || e.Args[4].Length != 43)
        {
            Shutdown(2);
            return;
        }

        try
        {
            await CoreConnection.RunAsync(e.Args[2], e.Args[4]);
            Shutdown();
        }
        catch (Exception error) when (error is IOException or UnauthorizedAccessException or
                                       OperationCanceledException or System.ComponentModel.Win32Exception or
                                       System.Text.Json.JsonException)
        {
            // The service observes the process exit and starts a replacement.
            Shutdown(1);
        }
    }
}
