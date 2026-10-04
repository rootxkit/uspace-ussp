import type { Metadata } from "next";
import { pageTitle } from "@/i18n/server";
import { ConsoleInputsPage } from "@/console/InputsPage";

export async function generateMetadata(): Promise<Metadata> {
  return { title: await pageTitle("console.title.inputs") };
}

export default function Page() {
  return <ConsoleInputsPage />;
}
