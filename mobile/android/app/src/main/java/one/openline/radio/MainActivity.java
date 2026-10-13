package one.openline.radio;

import android.os.Bundle;
import android.view.View;
import android.webkit.WebSettings;
import android.webkit.WebView;
import com.getcapacitor.BridgeActivity;

public class MainActivity extends BridgeActivity {
    @Override
    public void onCreate(Bundle savedInstanceState) {
        registerPlugin(RadioServicePlugin.class); // эфир в фоне — сервис с уведомлением
        super.onCreate(savedInstanceState);
        // масштаб страницы фиксирован: после окна выбора файлов WebView сам увеличивал страницу
        WebSettings ws = getBridge().getWebView().getSettings();
        ws.setSupportZoom(false);
        ws.setBuiltInZoomControls(false);
        ws.setDisplayZoomControls(false);
    }

    /**
     * Приложение ушло в фон: окно скрыто, и WebView (Chromium) ставит на паузу все <audio> страницы —
     * песня в эфире и приёмник замолкали, хотя сервис держит процесс. Пока сервис эфира работает,
     * говорим WebView, что окно видно. Скрытие приходит чуть позже onStop, поэтому — с задержкой и
     * повтором.
     */
    @Override
    public void onStop() {
        super.onStop();
        if (!RadioPlaybackService.running) return;
        WebView wv = getBridge().getWebView();
        Runnable keep = () -> {
            if (RadioPlaybackService.running) wv.dispatchWindowVisibilityChanged(View.VISIBLE);
        };
        wv.postDelayed(keep, 300);
        wv.postDelayed(keep, 1000);
    }
}
