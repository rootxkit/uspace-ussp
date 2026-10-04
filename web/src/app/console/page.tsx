import type { Metadata } from "next";
import { pageTitle } from "@/i18n/server";
import { ConsoleMapPage } from "@/console/MapPage";

export async function generateMetadata(): Promise<Metadata> {
  return { title: await pageTitle("console.title.map") };
}

export default function Page() {
  return <ConsoleMapPage />;
}
