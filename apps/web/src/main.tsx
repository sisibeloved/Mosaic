import { StrictMode } from "react";
import { createRoot } from "react-dom/client";
import { RouterProvider } from "react-router-dom";
import { router } from "./app/router";
import "./styles.css";

const root = document.getElementById("root");
if (!root) {
  throw new Error("缺少 #root 挂载点");
}
createRoot(root).render(
  <StrictMode>
    <RouterProvider router={router} />
  </StrictMode>,
);
