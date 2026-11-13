package one.openline.kontakt;

import android.os.Bundle;
import android.webkit.WebSettings;
import com.getcapacitor.BridgeActivity;

// Трубка «Контакта» — страница https://openline.one/ в WebView. Звонок (трель, вибрация через Haptics,
// качание трубки) делает сама страница; нативное — только микрофон, вибрация и фиксированный масштаб.
public class MainActivity extends BridgeActivity {
    @Override
    public void onCreate(Bundle savedInstanceState) {
        super.onCreate(savedInstanceState);
        WebSettings ws = getBridge().getWebView().getSettings();
        ws.setSupportZoom(false);
        ws.setBuiltInZoomControls(false);
        ws.setDisplayZoomControls(false);
        // звук без касания экрана (трель входящего) Capacitor уже разрешил: Bridge,
        // setMediaPlaybackRequiresUserGesture(false)
    }
}
