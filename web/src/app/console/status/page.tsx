import type { Metadata } from "next";
import { pageTitle } from "@/i18n/server";
import { ConsoleStatusPage } from "@/console/StatusPage";

export async function generateMetadata(): Promise<Metadata> {
  return { title: await pageTitle("console.title.status") };
}

export default function Page() {
  return <ConsoleStatusPage />;
}
