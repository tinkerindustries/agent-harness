// JetBrains Mono, self-hosted through @fontsource: Vite resolves the package's
// own CSS, which points at the woff2 files it ships, so the bundle carries the
// three latin faces and the UI makes no network request to a font CDN. The
// harness binds to loopback; a page that reaches the internet to render a
// session id is wrong.
import "@fontsource/jetbrains-mono/latin-400.css";
import "@fontsource/jetbrains-mono/latin-500.css";
import "@fontsource/jetbrains-mono/latin-700.css";
import { IconContext } from "@phosphor-icons/react";
import "highlight.js/styles/github.css";
import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import App from "./App";
import "./styles.css";

// Phosphor at light weight, once, rather than on forty call sites: at 16px its
// ~1px stroke matches the 1px --border hairline the whole UI is drawn from.
const icons = { weight: "light", size: 16 } as const;

createRoot(document.getElementById("root")!).render(
  <StrictMode>
    <IconContext.Provider value={icons}>
      <App />
    </IconContext.Provider>
  </StrictMode>,
);
