// bfbc - ReXGlue Recompiled Project

#pragma once

#include <rex/rex_app.h>

// mouse_look.cpp
void BfbcInstallMouseLook(rex::ui::Window* window);

class BfbcApp : public rex::ReXApp {
 public:
  using rex::ReXApp::ReXApp;

  static std::unique_ptr<rex::ui::WindowedApp> Create(
      rex::ui::WindowedAppContext& ctx) {
    return std::unique_ptr<BfbcApp>(new BfbcApp(ctx, "bfbc",
        PPCImageConfig));
  }

  void OnCreateDialogs(rex::ui::ImGuiDrawer* drawer) override {
    (void)drawer;
    BfbcInstallMouseLook(window());
  }
};
