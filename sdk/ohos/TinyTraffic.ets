import { bundleManager, common } from '@kit.AbilityKit';
import { preferences } from '@kit.ArkData';
import { deviceInfo } from '@kit.BasicServicesKit';
import { display } from '@kit.ArkUI';
import { i18n } from '@kit.LocalizationKit';
import { http } from '@kit.NetworkKit';
import { util } from '@kit.ArkTS';

/**
 * Privacy-friendly analytics for HarmonyOS apps, sending events to a
 * tinytraffic server. Copy this file into the app and call:
 *
 *   TinyTraffic.init(this.context, { endpoint: 'https://stats.example.com', appId: 'com.example.app' });
 *   TinyTraffic.screen('closet');
 *   TinyTraffic.track('outfit_saved', { 'items': 4 });
 *
 * The app needs the ohos.permission.INTERNET permission. ArkTS wants quoted
 * keys in prop objects: { 'items': 4 }.
 *
 * - The device ID is a random UUID in the app's own preferences. It goes away
 *   with the app's data, and nothing in it identifies the person or links
 *   apps together. The server hashes it with the app ID.
 * - Events are queued, sent in batches, and kept across launches until the
 *   server has taken them.
 * - Sessions follow the foreground: a new one starts after 30 minutes in the
 *   background.
 */

export type EventProps = Record<string, string | number | boolean>;

export interface TinyTrafficOptions {
  /** Base URL of the tinytraffic server, e.g. https://stats.example.com */
  endpoint: string;
  /** The app ID the site was registered with, usually the bundle name. */
  appId: string;
  /** Distribution channel, e.g. appgallery. */
  channel?: string;
  /** Development builds: nothing is recorded or sent. */
  debug?: boolean;
}

class QueuedEvent {
  name: string = '';
  screen: string = '';
  ts: number = 0;
  /** The session the event happened in. Kept so one request never mixes sessions. */
  session: string = '';
  props: EventProps = {};
}

class WireEvent {
  name: string = '';
  screen: string = '';
  ts: number = 0;
  props: EventProps = {};
}

class WireBatch {
  app_id: string = '';
  device_id: string = '';
  session_id: string = '';
  os: string = 'HarmonyOS';
  os_version: string = '';
  app_version: string = '';
  build: string = '';
  device_model: string = '';
  device_type: string = '';
  locale: string = '';
  screen_width: number = 0;
  channel: string = '';
  events: WireEvent[] = [];
}

const PREFS_NAME: string = 'tinytraffic';
const KEY_DEVICE_ID: string = 'device_id';
const KEY_QUEUE: string = 'queue';
const ENDPOINT_PATH: string = '/api/app/event';
/** Server limit per request is 100. */
const BATCH_SIZE: number = 50;
/** Send once this many events are waiting… */
const FLUSH_COUNT: number = 20;
/** …or this long after the first one. */
const FLUSH_DELAY_MS: number = 10000;
/** Oldest events are dropped beyond this, e.g. after a long time offline. */
const MAX_QUEUE: number = 500;
const SESSION_TIMEOUT_MS: number = 30 * 60 * 1000;

export class TinyTraffic {
  private static options: TinyTrafficOptions | undefined = undefined;
  private static prefs: preferences.Preferences | undefined = undefined;
  private static deviceId: string = '';
  private static sessionId: string = '';
  private static backgroundedAt: number = 0;
  private static queue: QueuedEvent[] = [];
  private static timer: number = -1;
  private static sending: boolean = false;
  private static on: boolean = true;

  /** Call once from EntryAbility.onCreate. Starts a session and records app_open. */
  static init(context: common.UIAbilityContext, options: TinyTrafficOptions): void {
    if (TinyTraffic.options !== undefined) {
      return;
    }
    TinyTraffic.options = options;
    try {
      TinyTraffic.prefs = preferences.getPreferencesSync(context, { name: PREFS_NAME });
    } catch (err) {
      TinyTraffic.prefs = undefined;
    }
    TinyTraffic.deviceId = TinyTraffic.loadDeviceId();
    TinyTraffic.sessionId = util.generateRandomUUID(false);
    TinyTraffic.queue = TinyTraffic.loadQueue();
    try {
      context.getApplicationContext().on('applicationStateChange', {
        onApplicationForeground: () => TinyTraffic.onForeground(),
        onApplicationBackground: () => TinyTraffic.onBackground()
      });
    } catch (err) {
      // Without lifecycle callbacks, sessions only restart with the process.
    }
    TinyTraffic.track('app_open');
  }

  /** Turn collection on or off, e.g. with the user's consent. While off, nothing is queued or sent. */
  static setEnabled(enabled: boolean): void {
    TinyTraffic.on = enabled;
  }

  /** Records that a screen was shown. Shows up under Pages. */
  static screen(name: string): void {
    TinyTraffic.record('screen_view', name, undefined);
  }

  /** Records a custom event. Shows up under Goals. Props are flattened to strings by the server. */
  static track(name: string, props?: EventProps, screen?: string): void {
    TinyTraffic.record(name, screen ?? '', props);
  }

  /** Sends everything queued now. */
  static flush(): void {
    TinyTraffic.clearTimer();
    TinyTraffic.send();
  }

  private static record(name: string, screen: string, props: EventProps | undefined): void {
    const opts = TinyTraffic.options;
    if (opts === undefined || opts.debug === true || !TinyTraffic.on) {
      return;
    }
    const e = new QueuedEvent();
    e.name = name;
    e.screen = screen;
    e.ts = Date.now();
    e.session = TinyTraffic.sessionId;
    if (props !== undefined) {
      e.props = props;
    }
    TinyTraffic.queue.push(e);
    if (TinyTraffic.queue.length > MAX_QUEUE) {
      TinyTraffic.queue.splice(0, TinyTraffic.queue.length - MAX_QUEUE);
    }
    TinyTraffic.persist();
    if (TinyTraffic.queue.length >= FLUSH_COUNT) {
      TinyTraffic.flush();
    } else if (TinyTraffic.timer < 0) {
      TinyTraffic.timer = setTimeout(() => {
        TinyTraffic.timer = -1;
        TinyTraffic.send();
      }, FLUSH_DELAY_MS);
    }
  }

  private static onForeground(): void {
    if (TinyTraffic.backgroundedAt > 0 && Date.now() - TinyTraffic.backgroundedAt >= SESSION_TIMEOUT_MS) {
      TinyTraffic.sessionId = util.generateRandomUUID(false);
      TinyTraffic.track('app_open');
    }
    TinyTraffic.backgroundedAt = 0;
    TinyTraffic.send();
  }

  private static onBackground(): void {
    TinyTraffic.backgroundedAt = Date.now();
    TinyTraffic.flush();
  }

  private static clearTimer(): void {
    if (TinyTraffic.timer >= 0) {
      clearTimeout(TinyTraffic.timer);
      TinyTraffic.timer = -1;
    }
  }

  /** Sends one batch, then goes on with the rest. A failed send stays queued for the next trigger. */
  private static send(): void {
    const opts = TinyTraffic.options;
    if (opts === undefined || opts.debug === true || TinyTraffic.sending || TinyTraffic.queue.length === 0) {
      return;
    }
    // One request, one session.
    const session = TinyTraffic.queue[0].session;
    const events: QueuedEvent[] = [];
    for (const e of TinyTraffic.queue) {
      if (e.session !== session || events.length >= BATCH_SIZE) {
        break;
      }
      events.push(e);
    }
    TinyTraffic.sending = true;
    TinyTraffic.post(opts, session, events)
      .then((done: boolean) => {
        TinyTraffic.sending = false;
        if (!done) {
          return;
        }
        TinyTraffic.queue.splice(0, events.length);
        TinyTraffic.persist();
        TinyTraffic.send();
      })
      .catch(() => {
        TinyTraffic.sending = false;
      });
  }

  /** Resolves true when the events can leave the queue: the server took them or refused them for good. */
  private static async post(opts: TinyTrafficOptions, session: string, events: QueuedEvent[]): Promise<boolean> {
    const batch = new WireBatch();
    batch.app_id = opts.appId;
    batch.device_id = TinyTraffic.deviceId;
    batch.session_id = session;
    batch.channel = opts.channel ?? '';
    batch.os_version = deviceInfo.distributionOSVersion || deviceInfo.displayVersion;
    batch.device_model = deviceInfo.marketName || deviceInfo.productModel;
    batch.device_type = deviceInfo.deviceType;
    batch.locale = TinyTraffic.locale();
    batch.screen_width = TinyTraffic.screenWidth();
    const info = TinyTraffic.bundle();
    batch.app_version = info[0];
    batch.build = info[1];
    for (const e of events) {
      const w = new WireEvent();
      w.name = e.name;
      w.screen = e.screen;
      w.ts = e.ts;
      w.props = e.props;
      batch.events.push(w);
    }

    const req = http.createHttp();
    try {
      const url = opts.endpoint.replace(/\/+$/, '') + ENDPOINT_PATH;
      const resp = await req.request(url, {
        method: http.RequestMethod.POST,
        header: { 'Content-Type': 'application/json' } as Record<string, string>,
        extraData: JSON.stringify(batch),
        expectDataType: http.HttpDataType.STRING,
        connectTimeout: 10000,
        readTimeout: 10000
      });
      const code = resp.responseCode;
      // Server errors and rate limits are worth another try; other refusals are not.
      return !(code === 429 || code >= 500);
    } catch (err) {
      return false;
    } finally {
      req.destroy();
    }
  }

  private static bundle(): string[] {
    try {
      const info = bundleManager.getBundleInfoForSelfSync(bundleManager.BundleFlag.GET_BUNDLE_INFO_DEFAULT);
      return [info.versionName, info.versionCode.toString()];
    } catch (err) {
      return ['', ''];
    }
  }

  private static locale(): string {
    try {
      return i18n.System.getSystemLocale();
    } catch (err) {
      return '';
    }
  }

  /** Width in vp, which is what separates phones from tablets. */
  private static screenWidth(): number {
    try {
      const d = display.getDefaultDisplaySync();
      return Math.round(d.width / d.densityPixels);
    } catch (err) {
      return 0;
    }
  }

  private static loadDeviceId(): string {
    const prefs = TinyTraffic.prefs;
    let id = '';
    try {
      id = prefs ? (prefs.getSync(KEY_DEVICE_ID, '') as string) : '';
      if (id.length === 0) {
        id = util.generateRandomUUID(false);
        if (prefs) {
          prefs.putSync(KEY_DEVICE_ID, id);
          prefs.flush();
        }
      }
    } catch (err) {
      id = id.length > 0 ? id : util.generateRandomUUID(false);
    }
    return id;
  }

  private static loadQueue(): QueuedEvent[] {
    try {
      const raw = TinyTraffic.prefs ? (TinyTraffic.prefs.getSync(KEY_QUEUE, '') as string) : '';
      return raw.length > 0 ? (JSON.parse(raw) as QueuedEvent[]) : [];
    } catch (err) {
      return [];
    }
  }

  private static persist(): void {
    try {
      if (TinyTraffic.prefs) {
        TinyTraffic.prefs.putSync(KEY_QUEUE, JSON.stringify(TinyTraffic.queue));
        TinyTraffic.prefs.flush();
      }
    } catch (err) {
      // Losing the on-disk copy only costs events after a crash.
    }
  }
}
