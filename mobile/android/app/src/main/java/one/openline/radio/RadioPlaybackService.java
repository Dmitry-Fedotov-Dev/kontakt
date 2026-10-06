package one.openline.radio;

import android.Manifest;
import android.app.Notification;
import android.app.NotificationChannel;
import android.app.NotificationManager;
import android.app.PendingIntent;
import android.app.Service;
import android.content.Intent;
import android.content.pm.PackageManager;
import android.content.pm.ServiceInfo;
import android.os.Build;
import android.os.IBinder;
import android.os.PowerManager;
import androidx.core.app.NotificationCompat;
import androidx.core.app.ServiceCompat;
import androidx.core.content.ContextCompat;

/**
 * Сервис переднего плана: пока он работает, Android не усыпляет процесс, и эфир в WebView
 * играет при погашенном экране и в фоне. Тип — воспроизведение медиа; когда ведущий в эфире,
 * ещё и микрофон (Android 14+ иначе отключает микрофон в фоне).
 */
public class RadioPlaybackService extends Service {
    static final String CHANNEL = "air";
    static final String ACTION_STOP = "one.openline.radio.STOP";
    private PowerManager.WakeLock wake;

    @Override
    public int onStartCommand(Intent intent, int flags, int startId) {
        if (intent != null && ACTION_STOP.equals(intent.getAction())) {
            if (RadioServicePlugin.instance != null) RadioServicePlugin.instance.stoppedFromNotification();
            stopSelf();
            return START_NOT_STICKY;
        }
        String title = intent != null ? intent.getStringExtra("title") : null;
        String text = intent != null ? intent.getStringExtra("text") : null;
        boolean mic = intent != null && intent.getBooleanExtra("mic", false);

        NotificationManager nm = getSystemService(NotificationManager.class);
        if (Build.VERSION.SDK_INT >= 26 && nm.getNotificationChannel(CHANNEL) == null) {
            NotificationChannel ch = new NotificationChannel(CHANNEL, getString(R.string.channel_air), NotificationManager.IMPORTANCE_LOW);
            ch.setShowBadge(false);
            nm.createNotificationChannel(ch);
        }
        int pf = PendingIntent.FLAG_UPDATE_CURRENT | PendingIntent.FLAG_IMMUTABLE;
        PendingIntent open = PendingIntent.getActivity(this, 0,
            new Intent(this, MainActivity.class).addFlags(Intent.FLAG_ACTIVITY_SINGLE_TOP), pf);
        PendingIntent stop = PendingIntent.getService(this, 1,
            new Intent(this, RadioPlaybackService.class).setAction(ACTION_STOP), pf);
        Notification n = new NotificationCompat.Builder(this, CHANNEL)
            .setSmallIcon(R.drawable.ic_stat_radio)
            .setContentTitle(title == null || title.isEmpty() ? getString(R.string.app_name) : title)
            .setContentText(text == null ? "" : text)
            .setContentIntent(open)
            .addAction(0, getString(R.string.action_stop), stop)
            .setOngoing(true)
            .setSilent(true)
            .setCategory(NotificationCompat.CATEGORY_TRANSPORT)
            .setVisibility(NotificationCompat.VISIBILITY_PUBLIC)
            .build();
        // Тип «микрофон» — только с выданным разрешением: иначе Android 14+ бросает SecurityException
        // и роняет приложение. Не вышло и так — сервис хотя бы проигрывателем, но не падаем.
        boolean micOk = mic && Build.VERSION.SDK_INT >= 30
            && ContextCompat.checkSelfPermission(this, Manifest.permission.RECORD_AUDIO) == PackageManager.PERMISSION_GRANTED;
        int play = Build.VERSION.SDK_INT >= 29 ? ServiceInfo.FOREGROUND_SERVICE_TYPE_MEDIA_PLAYBACK : 0;
        int type = micOk ? play | ServiceInfo.FOREGROUND_SERVICE_TYPE_MICROPHONE : play;
        try {
            ServiceCompat.startForeground(this, 1, n, type);
        } catch (RuntimeException e) {
            try {
                ServiceCompat.startForeground(this, 1, n, play);
            } catch (RuntimeException e2) {
                stopSelf();
                return START_NOT_STICKY;
            }
        }
        if (wake == null) {
            PowerManager pm = getSystemService(PowerManager.class);
            wake = pm.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, "openradio:air");
            wake.acquire();
        }
        return START_NOT_STICKY;
    }

    @Override
    public void onDestroy() {
        if (wake != null && wake.isHeld()) wake.release();
        wake = null;
        super.onDestroy();
    }

    @Override
    public IBinder onBind(Intent intent) {
        return null;
    }
}
