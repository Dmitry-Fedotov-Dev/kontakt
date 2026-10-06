package one.openline.radio;

import android.Manifest;
import android.content.Intent;
import android.os.Build;
import androidx.core.content.ContextCompat;
import com.getcapacitor.JSObject;
import com.getcapacitor.Plugin;
import com.getcapacitor.PluginCall;
import com.getcapacitor.PluginMethod;
import com.getcapacitor.annotation.CapacitorPlugin;
import com.getcapacitor.annotation.Permission;

/**
 * Мост страницы радио к сервису фонового эфира: страница зовёт start, когда звук включён
 * (слушает или ведёт эфир), и stop, когда выключен. Кнопка «Стоп» в уведомлении шлёт странице
 * событие "stop" — она глушит звук.
 */
@CapacitorPlugin(
    name = "RadioService",
    permissions = { @Permission(alias = "notifications", strings = { Manifest.permission.POST_NOTIFICATIONS }) }
)
public class RadioServicePlugin extends Plugin {
    static RadioServicePlugin instance;

    @Override
    public void load() {
        instance = this;
    }

    @PluginMethod
    public void start(PluginCall call) {
        Intent i = new Intent(getContext(), RadioPlaybackService.class);
        i.putExtra("title", call.getString("title", "Open Radio"));
        i.putExtra("text", call.getString("text", ""));
        i.putExtra("mic", call.getBoolean("mic", false));
        try {
            ContextCompat.startForegroundService(getContext(), i);
            call.resolve();
        } catch (RuntimeException e) { // например, запуск из фона запрещён — эфир идёт, без уведомления
            call.reject(e.getMessage());
        }
    }

    @PluginMethod
    public void stop(PluginCall call) {
        getContext().stopService(new Intent(getContext(), RadioPlaybackService.class));
        call.resolve();
    }

    void stoppedFromNotification() {
        notifyListeners("stop", new JSObject());
    }
}
