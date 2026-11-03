import { render } from "solid-js/web";
import "./app.css";
import { Client } from "./core/client";
import { App } from "./ui/App";

const client = new Client();
render(() => <App client={client} />, document.getElementById("root")!);
void client.start();
