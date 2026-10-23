import type { Metadata } from 'next';
import { Inter } from 'next/font/google';
import './globals.css';
import { Navbar } from '@/components/Navbar';

const inter = Inter({ subsets: ['latin'] });

export const metadata: Metadata = {
  title: 'Agent Workbench',
  description: 'Task execution foundation and monitoring dashboard',
};

export default function RootLayout({
  children,
}: {
  children: React.ReactNode;
}) {
  return (
    <html lang="en" className="dark">
      <body className={`${inter.className} bg-gray-950 text-gray-100 min-h-screen flex flex-col antialiased`}>
        <Navbar />
        <main className="pt-16 flex-1 flex flex-col">
          <div className="w-full flex-1 flex flex-col px-4 sm:px-6 py-4">
            {children}
          </div>
        </main>
      </body>
    </html>
  );
}
