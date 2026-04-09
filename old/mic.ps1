# Verifica se o microfone está sendo usado AGORA (ícone na barra de tarefas)

Add-Type -TypeDefinition @"
using System;
using System.Runtime.InteropServices;
using System.Collections.Generic;

namespace AudioMonitor {
    [ComImport]
    [Guid("BCDE0395-E52F-467C-8E3D-C4579291692E")]
    internal class MMDeviceEnumeratorClass { }

    [Guid("A95664D2-9614-4F35-A746-DE8DB63617E6")]
    [InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
    internal interface IMMDeviceEnumerator {
        int NotImpl1();
        int GetDefaultAudioEndpoint(int dataFlow, int role, out IMMDevice ppDevice);
    }

    [Guid("D666063F-1587-4E43-81F1-B948E807363F")]
    [InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
    internal interface IMMDevice {
        int Activate(ref Guid iid, int dwClsCtx, IntPtr pActivationParams, out IAudioMeterInformation ppInterface);
    }

    [Guid("C02216F6-8C67-4B5B-9D00-D008E73E0064")]
    [InterfaceType(ComInterfaceType.InterfaceIsIUnknown)]
    internal interface IAudioMeterInformation {
        int GetPeakValue(out float pfPeak);
    }

    public class MicrophoneMonitor {
        public static bool IsMicrophoneActive() {
            try {
                var enumerator = new MMDeviceEnumeratorClass() as IMMDeviceEnumerator;
                IMMDevice device;

                // 1 = eCapture (microfone), 0 = eConsole (default device)
                enumerator.GetDefaultAudioEndpoint(1, 0, out device);

                Guid IID_IAudioMeterInformation = new Guid("C02216F6-8C67-4B5B-9D00-D008E73E0064");
                IAudioMeterInformation meter;

                device.Activate(ref IID_IAudioMeterInformation, 1, IntPtr.Zero, out meter);

                float peak;
                meter.GetPeakValue(out peak);

                // Se peak > 0, o microfone está captando áudio
                return peak > 0.001f; // threshold pequeno para evitar ruído
            } catch {
                return false;
            }
        }
    }
}
"@ -ErrorAction SilentlyContinue

try {
    $isMicActive = [AudioMonitor.MicrophoneMonitor]::IsMicrophoneActive()

    if ($isMicActive) {
        # Microfone está ativo, identifica qual app
        $possibleProcesses = @(
            "Teams", "ms-teams", "Zoom", "Discord", "Skype", "Slack",
            "WhatsApp", "Telegram", "chrome", "firefox", "msedge",
            "webex", "gotomeeting", "bluejeans"
        )

        $activeProcesses = Get-Process | Where-Object {
            $processName = $_.ProcessName
            $possibleProcesses | Where-Object { $processName -match $_ }
        } | Select-Object -ExpandProperty ProcessName -Unique

        if ($activeProcesses.Count -gt 0) {
            Write-Output ($activeProcesses -join ",")
        } else {
            Write-Output "MICROPHONE_ACTIVE_UNKNOWN"
        }
    } else {
        Write-Output "NO_MICROPHONE_USAGE"
    }
} catch {
    Write-Output "ERROR: $($_.Exception.Message)"
}
