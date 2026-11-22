// bfbc - Direct mouse look for Battlefield: Bad Company
//
// The keyboard/mouse emulation in ReXGlue turns mouse motion into right-stick
// deflection, so the game applies its gamepad aim pipeline on top (dead zone,
// response curve, acceleration, max turn speed). This hook bypasses all of that:
// it adds raw mouse motion straight to the soldier's yaw/pitch angles.
//
// Reverse-engineering notes (title 454107F9, default.xex 0.0.0.8):
//   sub_8227DE90(state r3, input r4, flag r5) = soldier aiming update
//     state+0  : AimingControllerData*   (YawSpeed @100, PitchSpeed @88, ...)
//     state+8  : yaw   (radians, wrapped to [0, 2pi) by the function)
//     state+12 : pitch (radians, clamped by the function)
//     input+0  : delta time
//     input+12 : look input X (right stick, after the game's dead zone)
//     input+16 : look input Y
//     input+28 : turn-rate multiplier (zoom related)
//   yaw   += YawSpeed   * input[28] * dt * f(lookX)
//   pitch += PitchSpeed * input[28] * dt * f(lookY)

#include <rex/cvar.h>
#include <rex/hook.h>
#include <rex/logging.h>
#include <rex/ui/ui_event.h>
#include <rex/ui/window.h>
#include <rex/ui/window_listener.h>

#include <algorithm>
#include <atomic>
#include <chrono>
#include <cmath>
#include <cstring>
#include <mutex>
#include <cstdio>
#include <string>
#include <unordered_map>
#include <vector>

REXCVAR_DEFINE_BOOL(bfbc_mouse_look, false, "BFBC",
                    "Direct mouse look: mouse rotates the camera instead of emulating a stick");
REXCVAR_DEFINE_DOUBLE(bfbc_mouse_sens, 1.0, "BFBC", "Direct mouse look sensitivity")
    .range(0.05, 10.0);
REXCVAR_DEFINE_BOOL(bfbc_mouse_invert_y, false, "BFBC", "Invert vertical mouse look");
REXCVAR_DEFINE_BOOL(bfbc_mouse_invert_x, false, "BFBC", "Invert horizontal mouse look");
REXCVAR_DEFINE_DOUBLE(bfbc_vehicle_sens, 1.0, "BFBC", "Mouse sensitivity multiplier for vehicle weapons")
    .range(0.1, 10.0);
REXCVAR_DEFINE_DOUBLE(bfbc_zoom_sens, 1.0, "BFBC",
                      "Extra mouse sensitivity multiplier while aiming down sights / through a scope")
    .range(0.1, 10.0);
REXCVAR_DEFINE_DOUBLE(bfbc_vehicle_curve, 0.5, "BFBC",
                      "Vehicle weapon response exponent (1 = linear, <1 boosts small movements)")
    .range(0.2, 1.0);
REXCVAR_DEFINE_DOUBLE(bfbc_vehicle_min, 0.12, "BFBC",
                      "Smallest stick deflection sent for any mouse movement in vehicles (0-0.5)")
    .range(0.0, 0.5);
REXCVAR_DEFINE_BOOL(bfbc_weapon_keys, false, "BFBC",
                    "Number keys 1-5 select weapon / gadget slots (keyboard)");
REXCVAR_DEFINE_BOOL(bfbc_mouse_debug, false, "BFBC", "Log aim values for diagnostics");
REXCVAR_DEFINE_BOOL(bfbc_mouse_zoom_scale, true, "BFBC",
                    "Scale mouse look by the game's zoom turn-rate multiplier");

REXCVAR_DEFINE_UINT32(bfbc_mod_shift, 0, "BFBC",
                      "Gamepad buttons pressed while Shift is held (XInput mask, 0x10000=LT, 0x20000=RT)");
REXCVAR_DEFINE_UINT32(bfbc_mod_ctrl, 0, "BFBC", "Gamepad buttons pressed while Ctrl is held");
REXCVAR_DEFINE_UINT32(bfbc_mod_alt, 0, "BFBC", "Gamepad buttons pressed while Alt is held");
REXCVAR_DEFINE_UINT32(bfbc_mod_mouse4, 0, "BFBC", "Gamepad buttons pressed while mouse button 4 (back) is held");
REXCVAR_DEFINE_UINT32(bfbc_mod_mouse5, 0, "BFBC", "Gamepad buttons pressed while mouse button 5 (forward) is held");

extern "C" REX_FUNC(__imp__sub_8227DE90);
extern "C" REX_FUNC(__imp__sub_82181878);

#if defined(_WIN32)
#include <windows.h>  // already pulled in by rex/ui/window.h (presenter.h)
#endif

namespace {

constexpr double kDegPerCount = 0.022;  // like most PC shooters at sensitivity 1
constexpr double kDegToRad = 3.14159265358979323846 / 180.0;

// --- guest memory helpers (big-endian) --------------------------------------
inline uint8_t* GuestPtr(uint8_t* base, uint32_t addr) {
  return base + addr + (addr >= 0xE0000000u ? 0x1000u : 0u);
}
inline float LoadF32(uint8_t* base, uint32_t addr) {
  uint32_t v;
  std::memcpy(&v, GuestPtr(base, addr), 4);
  v = __builtin_bswap32(v);
  float f;
  std::memcpy(&f, &v, 4);
  return f;
}
inline void StoreF32(uint8_t* base, uint32_t addr, float f) {
  uint32_t v;
  std::memcpy(&v, &f, 4);
  v = __builtin_bswap32(v);
  std::memcpy(GuestPtr(base, addr), &v, 4);
}

uint64_t NowUs() {
  using namespace std::chrono;
  return duration_cast<microseconds>(steady_clock::now().time_since_epoch()).count();
}

uint64_t NowMs() {
  using namespace std::chrono;
  return duration_cast<milliseconds>(steady_clock::now().time_since_epoch()).count();
}

// --- mouse delta accumulation (UI thread -> guest thread) -------------------
std::mutex g_mouse_mutex;
double g_mouse_dx = 0.0;
double g_mouse_dy = 0.0;
double g_pad_dx = 0.0;  // same motion, consumed by the vehicle (stick) path
double g_pad_dy = 0.0;
std::atomic<uint64_t> g_last_mouse_ms{0};

// Modifier keys (Shift/Ctrl/Alt) cannot be bound on their own in ReXGlue's
// keyboard emulation: holding one also blocks every other binding (WASD stops
// working while sprinting). When a modifier is mapped to a gamepad button we hide
// it from the emulation and press the button ourselves (see sub_82181878 hook).
bool ModifierMapped(rex::ui::VirtualKey vk) {
  switch (vk) {
    case rex::ui::VirtualKey::kShift: return REXCVAR_GET(bfbc_mod_shift) != 0;
    case rex::ui::VirtualKey::kControl: return REXCVAR_GET(bfbc_mod_ctrl) != 0;
    case rex::ui::VirtualKey::kMenu: return REXCVAR_GET(bfbc_mod_alt) != 0;
    default: return false;
  }
}

bool KeyHeld(int vk) {
#if defined(_WIN32)
  DWORD pid = 0;
  GetWindowThreadProcessId(GetForegroundWindow(), &pid);
  if (pid != GetCurrentProcessId()) return false;  // game not focused
  return (GetAsyncKeyState(vk) & 0x8000) != 0;
#else
  (void)vk;
  return false;
#endif
}

class MouseLookListener final : public rex::ui::WindowInputListener {
 public:
  void OnKeyDown(rex::ui::KeyEvent& e) override {
    if (ModifierMapped(e.virtual_key())) e.set_handled(true);
  }
  void OnKeyUp(rex::ui::KeyEvent& e) override {
    if (ModifierMapped(e.virtual_key())) e.set_handled(true);
  }
  void OnMouseMove(rex::ui::MouseEvent& e) override {
    float dx = e.dx(), dy = e.dy();
    if (dx == 0.0f && dy == 0.0f) return;
    {
      std::lock_guard lock(g_mouse_mutex);
      g_mouse_dx += dx;
      g_mouse_dy += dy;
      g_pad_dx += dx;
      g_pad_dy += dy;
    }
    g_last_mouse_ms.store(NowMs(), std::memory_order_relaxed);
  }
};
MouseLookListener g_listener;

void TakeMouse(double& dx, double& dy) {
  std::lock_guard lock(g_mouse_mutex);
  dx = g_mouse_dx;
  dy = g_mouse_dy;
  g_mouse_dx = g_mouse_dy = 0.0;
}

// --- local player tracking ---------------------------------------------------
uint32_t g_player_state = 0;
uint64_t g_player_seen_ms = 0;
uint64_t g_last_switch_log_ms = 0;
bool g_logged_multi = false;
uint64_t g_next_debug_ms = 0;
uint64_t g_last_pad_us = 0;
double g_pad_vx = 0.0, g_pad_vy = 0.0;  // smoothed mouse velocity (counts/s)
uint64_t g_next_pad_debug_ms = 0;

// --- weapon slot keys (1-5) --------------------------------------------------
// Bad Company's own buttons: Y = melee (knife), RB = main weapon <-> grenade
// launcher / grenade (and back to the main weapon from an item), LB = kit items
// (from a weapon or the knife it takes the first item, then it cycles the items;
// it never goes back to a weapon). Picked-up gadgets (RPG...) join the LB cycle.
// Every weapon has its own aim state, so we learn the role of each aim state from
// the button that switched to it, and turn the number keys into button presses:
//   1 main weapon, 2 launcher/grenade, 3 knife, 4 first LB item, 5 second LB item
//   (5 again while on the second item = next item, for a third gadget).
constexpr uint16_t kBtnY = 0x8000, kBtnLB = 0x0100, kBtnRB = 0x0200;
enum Role : uint8_t { kRoleUnknown = 0, kRoleWeapon, kRoleKnife, kRoleItem };
std::atomic<uint32_t> g_state_changes{0};
std::unordered_map<uint32_t, uint8_t> g_role;  // aim state -> role
uint32_t g_primary_state = 0, g_launcher_state = 0, g_knife_state = 0;
bool g_primary_guess = false;       // primary only guessed (held at spawn)
std::vector<uint32_t> g_items;      // LB items in cycle order
bool g_items_closed = false;        // LB from the last item went back to the first
uint16_t g_last_press = 0;          // last Y/LB/RB press (ours or the player's)
uint32_t g_state_at_press = 0;      // aim state when it was pressed

uint8_t RoleOf(uint32_t s) {
  auto it = g_role.find(s);
  return it == g_role.end() ? kRoleUnknown : it->second;
}
bool IsItem(uint32_t s) { return RoleOf(s) == kRoleItem; }
int ItemIndex(uint32_t s) {
  for (size_t i = 0; i < g_items.size(); i++)
    if (g_items[i] == s) return int(i);
  return -1;
}
void SetItem(uint32_t s) {
  g_role[s] = kRoleItem;
  if (g_primary_state == s) g_primary_state = 0, g_primary_guess = false;
  if (g_launcher_state == s) g_launcher_state = 0;
  if (g_knife_state == s) g_knife_state = 0;
}
void SetWeapon(uint32_t s) {
  g_role[s] = kRoleWeapon;
  const int i = ItemIndex(s);
  if (i >= 0) g_items.erase(g_items.begin() + i), g_items_closed = false;
  if (g_knife_state == s) g_knife_state = 0;
}

void LogSlots(const char* what) {
  std::string items;
  for (uint32_t s : g_items) {
    char buf[12];
    std::snprintf(buf, sizeof(buf), " %08X", s);
    items += buf;
  }
  REXLOG_INFO("[slots] {}: main {:08X}{} launcher {:08X} knife {:08X} items[{}]{}", what, g_primary_state,
              g_primary_guess ? "?" : "", g_launcher_state, g_knife_state, items,
              g_items_closed ? " (full cycle)" : "");
}

void OnAimStateChanged(uint32_t from, uint32_t to) {
  g_state_changes.fetch_add(1);
  // After a break in aiming (spawn, respawn, cutscene) aim states can be new. If
  // the state is one we know, it is the same soldier; otherwise start over. The
  // soldier usually holds the main weapon then, but not always (checkpoints), so
  // that is only a guess that later presses correct.
  if (!from || (NowMs() - g_player_seen_ms > 3000 && !g_role.count(to))) {
    g_role.clear();
    g_items.clear();
    g_items_closed = false;
    g_launcher_state = g_knife_state = 0;
    g_primary_state = to;
    g_primary_guess = true;
    g_last_press = 0;
    REXLOG_INFO("[slots] new soldier, holding {:08X}", to);
    return;
  }
  // Learn from the press that caused this switch.
  if (g_last_press && g_state_at_press == from) {
    const uint8_t from_role = RoleOf(from);
    if (g_last_press == kBtnY) {
      g_knife_state = to;
      g_role[to] = kRoleKnife;
      if (g_primary_state == to) g_primary_state = 0;
      if (g_launcher_state == to) g_launcher_state = 0;
      const int i = ItemIndex(to);
      if (i >= 0) g_items.erase(g_items.begin() + i), g_items_closed = false;
    } else if (g_last_press == kBtnLB) {
      // LB always lands on an item. From a weapon / the knife: the first item.
      SetItem(to);
      if (from_role != kRoleItem) {
        if (g_items.empty() || g_items[0] != to) {
          g_items.clear();
          g_items_closed = false;
          g_items.push_back(to);
        }
      } else {
        const int fi = ItemIndex(from);
        const int ti = ItemIndex(to);
        if (fi < 0) {
          // Unknown position of 'from': start the list here.
          g_items.clear();
          g_items_closed = false;
          g_items.push_back(from);
          g_items.push_back(to);
        } else if (ti < 0) {
          // New item after 'from' (a picked-up gadget joins the cycle).
          g_items.insert(g_items.begin() + fi + 1, to);
          g_items_closed = false;
        } else if (ti == 0 && fi == int(g_items.size()) - 1) {
          g_items_closed = true;
        } else if (ti != fi + 1 && ti != 0) {
          // Order changed (item lost / swapped): keep what we know is adjacent.
          g_items.erase(g_items.begin() + ti);
          const int nfi = ItemIndex(from);
          g_items.insert(g_items.begin() + nfi + 1, to);
          g_items_closed = false;
        }
      }
    } else if (g_last_press == kBtnRB) {
      // RB: main weapon <-> launcher, and from the knife / an item back to main.
      if (from_role == kRoleItem || from_role == kRoleKnife) {
        g_primary_state = to;
        g_primary_guess = false;
        if (g_launcher_state == to) g_launcher_state = 0;
      } else if (to == g_primary_state) {
        g_launcher_state = from;
        g_primary_guess = false;
        SetWeapon(from);
      } else if (to == g_launcher_state || from == g_primary_state) {
        if (to == g_launcher_state) {
          g_primary_state = from;
        } else {
          g_launcher_state = to;
        }
        g_primary_guess = false;
        SetWeapon(from);
      } else {
        g_primary_state = from;
        g_launcher_state = to;
        g_primary_guess = true;
        SetWeapon(from);
      }
      SetWeapon(to);
    }
    LogSlots(g_last_press == kBtnY ? "learned Y" : g_last_press == kBtnLB ? "learned LB" : "learned RB");
  }
  g_last_press = 0;
}

struct SlotMacro {
  int target = 0;              // 0 idle, else key 1..5
  int phase = 0;               // 0 = go to main weapon, 1 = final presses
  int final_left = 0;          // presses left in phase 1
  int attempts = 0;
  uint16_t button = 0;         // button being pressed now
  int hold_polls = 0;          // polls left to hold the button
  int release_polls = 0;       // polls to keep it released afterwards
  uint32_t changes_at_press = 0;
  uint64_t wait_until_ms = 0;  // wait for the switch until then
  uint64_t give_up_ms = 0;
};
SlotMacro g_macro;
bool g_num_down[5] = {};
double g_sign_x = 1.0, g_sign_y = 1.0;
bool g_logged_inputs = false;
bool g_logged_first_call = false;
uint64_t g_calls = 0;
uint64_t g_next_report_ms = 0;
std::unordered_map<uint32_t, uint64_t> g_states_seen;  // state -> calls (diagnostics)

}  // namespace

// Called by the app once the window exists.
void BfbcInstallMouseLook(rex::ui::Window* window) {
  if (window) window->AddInputListener(&g_listener, 1u << 30);
  REXLOG_INFO("[mouse] installed (window={}, mouse_look={}, sens={})", window != nullptr,
              REXCVAR_GET(bfbc_mouse_look), REXCVAR_GET(bfbc_mouse_sens));
}

REX_HOOK_RAW(sub_8227DE90) {
  const uint32_t state = ctx.r3.u32;
  const uint32_t input = ctx.r4.u32;

  if (!g_logged_first_call) {
    g_logged_first_call = true;
    REXLOG_INFO("[mouse] aim hook active (state={:08X}, mouse_look={})", state,
                REXCVAR_GET(bfbc_mouse_look));
  }
  {
    const uint64_t t = NowMs();
    g_calls++;
    g_states_seen[state]++;
    if (!g_player_state && t >= g_next_report_ms) {
      g_next_report_ms = t + 3000;
      REXLOG_INFO("[mouse] waiting for player: {} calls, {} aim states", g_calls, g_states_seen.size());
    }
  }

  if (!REXCVAR_GET(bfbc_mouse_look) || !state || !input) {
    __imp__sub_8227DE90(ctx, base);
    return;
  }

  const uint64_t now = NowMs();

  // This aim update only runs for entities driven by the local player's input
  // (soldier weapons, vehicle weapons/turrets); AI uses another path. In logs only
  // one state is updated per frame, but it changes on weapon switch, entering a
  // vehicle, switching seats, respawning... So we no longer lock onto one state:
  // every call gets the mouse motion collected since the previous call.
  if (state != g_player_state) {
    if (now - g_last_switch_log_ms > 250) {
      REXLOG_INFO("[mouse] aim state {:08X} -> {:08X}", g_player_state, state);
      g_last_switch_log_ms = now;
    }
    // Two different states within the same frame (~10 ms) would mean two
    // player-controlled aim updates per frame; log it once for diagnostics.
    if (g_player_state && now - g_player_seen_ms < 10 && !g_logged_multi) {
      g_logged_multi = true;
      REXLOG_INFO("[mouse] note: states {:08X} and {:08X} updated in the same frame", g_player_state, state);
    }
    OnAimStateChanged(g_player_state, state);
    g_player_state = state;
    g_sign_x = REXCVAR_GET(bfbc_mouse_invert_x) ? -1.0 : 1.0;
    g_sign_y = -1.0;
  }

  // --- local player: direct mouse look ---
  double dx, dy;
  TakeMouse(dx, dy);
  // Motion collected while no aim update ran (menus, driving, cutscenes) must not
  // turn the camera in one jump when aiming resumes.
  if (now - g_player_seen_ms > 200) dx = dy = 0.0;
  g_player_seen_ms = now;

  if (!g_logged_inputs) {
    g_logged_inputs = true;
    REXLOG_INFO("[mouse] input dt={} mul={}", LoadF32(base, input + 0), LoadF32(base, input + 28));
  }

  double scale = kDegPerCount * kDegToRad * REXCVAR_GET(bfbc_mouse_sens);
  if (REXCVAR_GET(bfbc_mouse_zoom_scale)) {
    // The weapon's ZoomLevelData.LookSpeedMultiplier: 0.7 hip, 0.2 iron sights,
    // 0.05-0.1 sniper scopes (the M95 / SV98 use exactly 0.05).
    const float mul = LoadF32(base, input + 28);
    if (std::isfinite(mul) && mul > 0.001f && mul < 1.0f) {
      scale *= mul;
      if (mul < 0.6f) scale *= REXCVAR_GET(bfbc_zoom_sens);  // zoomed in
    }
  }
  const double inv_y = REXCVAR_GET(bfbc_mouse_invert_y) ? -1.0 : 1.0;

  const float yaw0 = LoadF32(base, state + 8);
  const float pitch0 = LoadF32(base, state + 12);
  const float stick_x = LoadF32(base, input + 12);
  const float stick_y = LoadF32(base, input + 16);
  if (dx != 0.0 || dy != 0.0) {
    StoreF32(base, state + 8, float(yaw0 + g_sign_x * dx * scale));
    StoreF32(base, state + 12, float(pitch0 + g_sign_y * dy * scale * inv_y));
  }
  const float yaw1 = LoadF32(base, state + 8);
  const float pitch1 = LoadF32(base, state + 12);

  // Remove the stick-emulated look input so the game adds nothing on top.
  // The function itself wraps yaw and clamps pitch to the soldier's limits.
  StoreF32(base, input + 12, 0.0f);
  StoreF32(base, input + 16, 0.0f);
  __imp__sub_8227DE90(ctx, base);

  // Diagnostics: what the game did with our angles (vehicle aiming issues).
  if (REXCVAR_GET(bfbc_mouse_debug) && (dx != 0.0 || dy != 0.0) && now >= g_next_debug_ms) {
    g_next_debug_ms = now + 200;
    REXLOG_INFO(
        "[mouse] dbg st={:08X} d=({:.0f},{:.0f}) mul={:.3f} stick=({:.2f},{:.2f}) yaw {:.4f}->{:.4f}->{:.4f} "
        "pitch {:.4f}->{:.4f}->{:.4f}",
        state, dx, dy, LoadF32(base, input + 28), stick_x, stick_y, yaw0, yaw1, LoadF32(base, state + 8),
        pitch0, pitch1, LoadF32(base, state + 12));
  }
}

// Plans the next button press for a number key and returns the buttons to hold
// this poll. Called from the pad update (~30 times per second).
uint16_t RunSlotMacro() {
  const uint64_t now = NowMs();
  // Some items (repair tool...) have no aim state, so "on foot" is loose here.
  const bool on_foot = now - g_player_seen_ms < 3000 && g_player_state;
  for (int i = 0; i < 5; i++) {
    const bool down = KeyHeld('1' + i);
    if (down && !g_num_down[i] && on_foot) {
      g_macro = SlotMacro{};
      g_macro.target = i + 1;
      g_macro.give_up_ms = now + 4000;
      REXLOG_INFO("[slots] key {} (state {:08X})", i + 1, g_player_state);
      LogSlots("known");
    }
    g_num_down[i] = down;
  }
  SlotMacro& m = g_macro;
  if (!m.target) return 0;
  if (now > m.give_up_ms) {
    REXLOG_INFO("[slots] gave up on key {}", m.target);
    m = SlotMacro{};
    return 0;
  }
  if (m.hold_polls > 0) {
    m.hold_polls--;
    if (m.hold_polls == 0) m.release_polls = 2;
    return m.button;
  }
  if (m.release_polls > 0) {  // let the button go before the next press
    m.release_polls--;
    return 0;
  }
  // After a press: wait for the aim state to change (or a short time-out, items
  // without an aim state never report a change).
  if (m.wait_until_ms && g_state_changes.load() == m.changes_at_press) {
    if (now < m.wait_until_ms) return 0;
    // The press switched nothing we can see (no aim state, or blocked): stop here
    // instead of pressing again and again.
    REXLOG_INFO("[slots] key {}: no switch after the press, stopping (state {:08X})", m.target, g_player_state);
    g_last_press = 0;
    m = SlotMacro{};
    return 0;
  }
  m.wait_until_ms = 0;

  const uint32_t cur = g_player_state;
  const uint8_t role = RoleOf(cur);
  const bool is_item = role == kRoleItem;
  const bool is_knife = role == kRoleKnife || (g_knife_state && cur == g_knife_state);
  const bool on_launcher = g_launcher_state && cur == g_launcher_state;
  const bool on_weapon = !is_item && !is_knife;  // main weapon or launcher (or unknown)
  uint16_t next = 0;
  const int max_presses = m.target >= 4 ? std::max<int>(4, int(g_items.size()) + 2) : 3;
  if (m.attempts >= max_presses) {
    REXLOG_INFO("[slots] key {} done after {} presses (state {:08X})", m.target, m.attempts, cur);
    m = SlotMacro{};
    return 0;
  }
  m.attempts++;
  switch (m.target) {
    case 1:  // main weapon
      if (!on_weapon || on_launcher) next = kBtnRB;
      else if (g_primary_state && cur != g_primary_state && m.attempts == 1) next = kBtnRB;
      break;
    case 2:  // launcher / grenade
      if (!on_weapon) next = kBtnRB;  // first back to the main weapon
      else if (!on_launcher && m.attempts <= 2) next = kBtnRB;
      break;
    case 3:  // knife
      if (!is_knife && m.attempts == 1) next = kBtnY;
      break;
    case 4:  // first item
      if (!is_item || (!g_items.empty() && cur != g_items[0])) next = kBtnLB;
      break;
    case 5: {  // second item; pressed again on the second item: the next one
      const int idx = is_item ? ItemIndex(cur) : -1;
      if (!is_item || idx <= 0) {
        next = kBtnLB;  // weapon -> first item -> second item
      } else if (m.attempts == 1 && (idx >= 2 || !g_items_closed || g_items.size() > 2)) {
        next = kBtnLB;  // step on to a third gadget (RPG...) and keep cycling
      }
      break;
    }
  }
  if (!next) {
    REXLOG_INFO("[slots] key {} done (state {:08X})", m.target, g_player_state);
    m = SlotMacro{};
    return 0;
  }
  g_last_press = next;
  g_state_at_press = cur;
  m.button = next;
  m.hold_polls = 3;  // ~100 ms press
  m.changes_at_press = g_state_changes.load();
  m.wait_until_ms = now + 100 + 650;
  return next;
}

// Pad state post-processing (dead zone) - called every time the game polls the
// controller with r4 = XINPUT_GAMEPAD (big-endian): +0 wButtons, +2 LT, +3 RT.
REX_HOOK_RAW(sub_82181878) {
  uint32_t mask = 0;
  if (REXCVAR_GET(bfbc_mod_shift) && KeyHeld(0x10)) mask |= REXCVAR_GET(bfbc_mod_shift);
  if (REXCVAR_GET(bfbc_mod_ctrl) && KeyHeld(0x11)) mask |= REXCVAR_GET(bfbc_mod_ctrl);
  if (REXCVAR_GET(bfbc_mod_alt) && KeyHeld(0x12)) mask |= REXCVAR_GET(bfbc_mod_alt);
  // Mouse side buttons (VK_XBUTTON1/2): ReXGlue's keybinds only know LMB/RMB/MMB.
  if (REXCVAR_GET(bfbc_mod_mouse4) && KeyHeld(0x05)) mask |= REXCVAR_GET(bfbc_mod_mouse4);
  if (REXCVAR_GET(bfbc_mod_mouse5) && KeyHeld(0x06)) mask |= REXCVAR_GET(bfbc_mod_mouse5);
  if (mask) {
    uint8_t* p = GuestPtr(base, ctx.r4.u32);
    uint16_t buttons = uint16_t((p[0] << 8) | p[1]);
    buttons |= uint16_t(mask & 0xFFFF);
    p[0] = uint8_t(buttons >> 8);
    p[1] = uint8_t(buttons & 0xFF);
    if (mask & 0x10000) p[2] = 0xFF;
    if (mask & 0x20000) p[3] = 0xFF;
  }
  // Vehicle weapons (mounted guns, turrets) do not go through the soldier aim
  // update; they read the right stick as a turn rate. ReXGlue's mouse emulation
  // turns "mouse counts since the last poll" into a stick value, which is
  // quantized and jumps between polls, and the game then applies a dead zone.
  // While no soldier aim update runs (= in a vehicle), we replace the right stick
  // after the dead zone with a smoothed mouse velocity instead.
  uint8_t* gp = GuestPtr(base, ctx.r4.u32);  // XINPUT_GAMEPAD, +8 sThumbRX, +10 sThumbRY (BE)
  if (REXCVAR_GET(bfbc_weapon_keys)) {
    uint16_t btn = uint16_t((gp[0] << 8) | gp[1]);
    // Learn from the player's own Y / LB / RB presses too.
    static uint16_t prev_btn = 0;
    const uint16_t pressed = btn & ~prev_btn & (kBtnY | kBtnLB | kBtnRB);
    prev_btn = btn;
    if (!g_macro.target && pressed) {
      g_last_press = (pressed & kBtnY) ? kBtnY : (pressed & kBtnRB) ? kBtnRB : kBtnLB;
      g_state_at_press = g_player_state;
    }
    const uint16_t extra = RunSlotMacro();
    if (extra) {
      btn |= extra;
      gp[0] = uint8_t(btn >> 8);
      gp[1] = uint8_t(btn & 0xFF);
    }
  }
  __imp__sub_82181878(ctx, base);
  if (!REXCVAR_GET(bfbc_mouse_look)) return;
  const uint64_t now_us = NowUs();
  double dx, dy;
  {
    std::lock_guard lock(g_mouse_mutex);
    dx = g_pad_dx;
    dy = g_pad_dy;
    g_pad_dx = g_pad_dy = 0.0;
  }
  double dt = g_last_pad_us ? double(now_us - g_last_pad_us) * 1e-6 : 0.0;
  g_last_pad_us = now_us;
  const bool soldier_aiming = NowMs() - g_player_seen_ms < 150;
  if (soldier_aiming || dt <= 0.0) {
    g_pad_vx = g_pad_vy = 0.0;
    return;
  }
  dt = std::clamp(dt, 0.004, 0.1);
  // counts per second, smoothed over ~40 ms
  const double a = 1.0 - std::exp(-dt / 0.04);
  g_pad_vx += a * (dx / dt - g_pad_vx);
  g_pad_vy += a * (dy / dt - g_pad_vy);
  if (NowMs() - g_last_mouse_ms.load(std::memory_order_relaxed) > 80) {
    g_pad_vx = g_pad_vy = 0.0;  // mouse idle: leave the stick to the keyboard binds
    return;
  }
  // Same overall gain as the mouse emulation at 30 polls/s (200 per count/poll).
  const double k = 200.0 * REXCVAR_GET(bfbc_mouse_sens) * REXCVAR_GET(bfbc_vehicle_sens) / 30.0;
  const double inv_y = REXCVAR_GET(bfbc_mouse_invert_y) ? -1.0 : 1.0;
  // The game's vehicle weapons are physics bodies driven by torque; small stick
  // values barely move them. Shape the response: any real mouse movement gives at
  // least bfbc_vehicle_min, and the curve lifts small values (sqrt by default).
  const double vmin = REXCVAR_GET(bfbc_vehicle_min);
  const double curve = REXCVAR_GET(bfbc_vehicle_curve);
  auto shape = [&](double v) -> int16_t {
    double n = std::clamp(std::fabs(v) / 32767.0, 0.0, 1.0);
    if (n < 0.004) return 0;  // ~ no movement (smoothing tail)
    n = vmin + (1.0 - vmin) * std::pow(n, curve);
    return int16_t(std::copysign(std::min(n, 1.0) * 32767.0, v));
  };
  const int16_t rx = shape(g_pad_vx * k);
  const int16_t ry = shape(-g_pad_vy * k * inv_y);
  gp[8] = uint8_t(uint16_t(rx) >> 8);
  gp[9] = uint8_t(uint16_t(rx) & 0xFF);
  gp[10] = uint8_t(uint16_t(ry) >> 8);
  gp[11] = uint8_t(uint16_t(ry) & 0xFF);
  if (REXCVAR_GET(bfbc_mouse_debug) && NowMs() >= g_next_pad_debug_ms) {
    g_next_pad_debug_ms = NowMs() + 250;
    REXLOG_INFO("[mouse] vehicle stick d=({:.0f},{:.0f}) dt={:.1f}ms v=({:.0f},{:.0f}) out=({},{})", dx, dy,
                dt * 1000.0, g_pad_vx, g_pad_vy, rx, ry);
  }
}
