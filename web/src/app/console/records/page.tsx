import type { Metadata } from "next";
import { pageTitle } from "@/i18n/server";
import { ConsoleRecordsPage } from "@/console/RecordsPages";

export async function generateMetadata(): Promise<Metadata> {
  return { title: await pageTitle("console.title.records") };
}

export default function Page() {
  return <ConsoleRecordsPage />;
}
