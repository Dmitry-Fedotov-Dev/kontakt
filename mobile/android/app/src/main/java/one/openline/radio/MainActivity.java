package one.openline.radio;

import android.os.Bundle;
import android.webkit.WebSettings;
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
}
