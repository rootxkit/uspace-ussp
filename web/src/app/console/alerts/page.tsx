import type { Metadata } from "next";
import { pageTitle } from "@/i18n/server";
import { ConsoleAlertsPage } from "@/console/AlertsPage";

export async function generateMetadata(): Promise<Metadata> {
  return { title: await pageTitle("console.title.alerts") };
}

export default function Page() {
  return <ConsoleAlertsPage />;
}
