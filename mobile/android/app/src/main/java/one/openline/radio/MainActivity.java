package one.openline.radio;

import android.os.Bundle;
import com.getcapacitor.BridgeActivity;

public class MainActivity extends BridgeActivity {
    @Override
    public void onCreate(Bundle savedInstanceState) {
        registerPlugin(RadioServicePlugin.class); // эфир в фоне — сервис с уведомлением
        super.onCreate(savedInstanceState);
    }
}
