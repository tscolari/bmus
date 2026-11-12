import { render } from "preact";
import { App } from "./app.js";
import "./app.css";

const root = document.getElementById("app");
if (root) render(<App />, root);
