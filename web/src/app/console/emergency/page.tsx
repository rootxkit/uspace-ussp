import type { Metadata } from "next";
import { pageTitle } from "@/i18n/server";
import { ConsoleEmergencyPage } from "@/console/EmergencyPages";

export async function generateMetadata(): Promise<Metadata> {
  return { title: await pageTitle("console.title.emergency") };
}

export default function Page() {
  return <ConsoleEmergencyPage />;
}
